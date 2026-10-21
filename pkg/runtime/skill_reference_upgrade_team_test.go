package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

var errCaptureTeamSkillUpgrade = errors.New("capture reviewed Team upgrade mutation")

type capturingTeamSkillUpgradeStore struct {
	SkillReferenceUpgradeStore
	kernelteam.Store
	mutation *SkillReferenceUpgradeMutation
}

func (s *capturingTeamSkillUpgradeStore) ApplySkillReferenceUpgrade(_ context.Context, mutation *SkillReferenceUpgradeMutation) error {
	s.mutation = mutation
	return errCaptureTeamSkillUpgrade
}

func teamSkillUpgradeRuntimeFixture(t *testing.T, store KernelStore) (*SkillReferenceUpgradeService, *capturingTeamSkillUpgradeStore, *SkillReferenceUpgradePlan) {
	t.Helper()
	ctx := t.Context()
	catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if err := catalog.Register(ctx, &skill.Definition{
			ID: "reader", Version: version, Name: "Reader", Requirements: skill.Requirements{AlwaysAvailable: true},
			Transport: skill.TransportReference{Kind: "tool", Endpoint: "reader"},
			Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read", Risk: skill.RiskLevelRead,
				SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "team-upgrade"}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "account", Scope: scope, DeploymentID: "team", SkillID: "reader", SkillVersion: "1.0.0",
		AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	teamStore := store.(kernelteam.Store)
	definition, err := kernelteam.PrepareDefinition(&kernelteam.Definition{
		ID: "team-design", Version: "1", DisplayName: "Team", Purpose: "Read sources",
		Roles: []kernelteam.RoleSlot{
			{ID: "reader", DisplayName: "Reader", Purpose: "Read", SkillGrants: []kernelteam.RoleSkillGrant{{SkillID: "reader", SkillVersion: "1.0.0", CatalogID: "native-reader", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead}}},
			{ID: "observer", DisplayName: "Observer", Purpose: "Observe without Skill authority", ChannelParticipation: kernelteam.RoleChannelObserveOnly},
		},
		Coordination: kernelteam.CoordinationPolicy{QuietByDefault: true, SuppressDuplicateContent: true},
		Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	}, eventWaitContractEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := teamStore.CreateTeamDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	deployment := &kernelteam.Deployment{ID: "team", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		Status: kernelteam.DeploymentActive, Revision: 1, CreatedAt: eventWaitContractEpoch, UpdatedAt: eventWaitContractEpoch}
	if err := teamStore.CreateTeamDeployment(ctx, deployment, workforce.DefinitionActivation{ID: "initial-team", Scope: scope, DeploymentID: deployment.ID,
		DefinitionID: definition.ID, ToVersion: definition.Version, DeploymentRevision: 1, ActorType: "system", ActorID: "fixture", CreatedAt: eventWaitContractEpoch}); err != nil {
		t.Fatal(err)
	}
	wrapped := &capturingTeamSkillUpgradeStore{SkillReferenceUpgradeStore: store.(SkillReferenceUpgradeStore), Store: teamStore}
	service := NewSkillReferenceUpgradeService(wrapped, catalog)
	service.now = func() time.Time { return eventWaitContractEpoch.Add(time.Minute) }
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{Scope: Scope(scope), DeploymentID: deployment.ID, BindingID: "account", ToVersion: "1.1.0"})
	if err != nil || plan.TeamAuthority == nil || plan.ApprovalRequired {
		t.Fatalf("automatic Team plan: %#v %v", plan, err)
	}
	if _, err := service.Apply(ctx, ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Adopt compatible latest Skill"}); !errors.Is(err, errCaptureTeamSkillUpgrade) || wrapped.mutation == nil {
		t.Fatalf("capture canonical mutation: %v", err)
	}
	return service, wrapped, plan
}

func assertTeamSkillUpgradeUnchanged(t *testing.T, store KernelStore, plan *SkillReferenceUpgradePlan) {
	t.Helper()
	binding, err := skill.NewCatalogWithStore(store.(skill.CatalogStore)).GetBinding(t.Context(), skill.ScopeReference(plan.Scope), plan.DeploymentID, plan.BindingID)
	if err != nil || binding.SkillVersion != plan.From.Version || binding.Revision != plan.ExpectedBindingRevision {
		t.Fatalf("binding changed on rejected Team transaction: %#v %v", binding, err)
	}
	versions, err := store.(kernelteam.Store).ListTeamDefinitionVersions(t.Context(), plan.TeamAuthority.DefinitionID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("rejected transaction leaked immutable definition: %#v %v", versions, err)
	}
	activations, err := store.(kernelteam.Store).ListTeamDefinitionActivations(t.Context(), capability.ScopeReference(plan.Scope), plan.DeploymentID)
	if err != nil || len(activations) != 1 {
		t.Fatalf("rejected transaction leaked activation: %#v %v", activations, err)
	}
}

func TestTeamSkillReferenceUpgradeStoreFencesConcurrentAuthorityAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		_, wrapped, plan := teamSkillUpgradeRuntimeFixture(t, store)
		teamStore := store.(kernelteam.Store)
		deployment, err := teamStore.GetTeamDeployment(t.Context(), capability.ScopeReference(plan.Scope), plan.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		deployment.Status, deployment.Revision = kernelteam.DeploymentPaused, deployment.Revision+1
		activation := workforce.DefinitionActivation{ID: "paused-team", Scope: deployment.Scope, DeploymentID: deployment.ID, DefinitionID: deployment.DefinitionID,
			FromVersion: deployment.ActiveVersion, ToVersion: deployment.ActiveVersion, DeploymentRevision: deployment.Revision, ActorType: "user", ActorID: "operator", CreatedAt: eventWaitContractEpoch.Add(time.Second)}
		if err := teamStore.UpdateTeamDeployment(t.Context(), deployment, plan.TeamAuthority.ExpectedRevision, activation); err != nil {
			t.Fatal(err)
		}
		if err := store.(SkillReferenceUpgradeStore).ApplySkillReferenceUpgrade(t.Context(), wrapped.mutation); !errors.Is(err, ErrSkillReferenceUpgradeConflict) {
			t.Fatalf("stale Team authority committed: %v", err)
		}
		binding, _ := skill.NewCatalogWithStore(store.(skill.CatalogStore)).GetBinding(t.Context(), skill.ScopeReference(plan.Scope), plan.DeploymentID, plan.BindingID)
		versions, _ := teamStore.ListTeamDefinitionVersions(t.Context(), deployment.DefinitionID)
		if binding.SkillVersion != plan.From.Version || binding.Revision != 1 || len(versions) != 1 {
			t.Fatalf("partial Team transaction: binding %#v versions %#v", binding, versions)
		}
	})
}

func TestTeamSkillReferenceUpgradeStoreDrainsBeforeAuthorityMutationAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		_, wrapped, plan := teamSkillUpgradeRuntimeFixture(t, store)
		run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: plan.Scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: plan.DeploymentID}, AssignedAgentID: "reader-agent", Goal: "Read sources", Source: RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		seedSkillUsageMetadata(t, store, run, &ActionCall{ID: "queued-old-team", Scope: plan.Scope, RunID: run.ID, DeploymentID: plan.DeploymentID,
			BindingID: plan.BindingID, BindingRevision: 1, SkillID: plan.From.ID, SkillVersion: plan.From.Version, Status: ActionCallStatusReady})
		if err := store.(SkillReferenceUpgradeStore).ApplySkillReferenceUpgrade(t.Context(), wrapped.mutation); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("Team authority changed before drain: %v", err)
		}
		assertTeamSkillUpgradeUnchanged(t, store, plan)
	})
}

func TestTeamSkillReferenceUpgradeRejectsConflictingImmutableVersionAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		_, wrapped, plan := teamSkillUpgradeRuntimeFixture(t, store)
		conflicting := cloneUpgradeTeamDefinition(wrapped.mutation.TeamAuthority.Definition)
		conflicting.Purpose = "Different immutable authority"
		conflicting, err := kernelteam.PrepareDefinition(conflicting, eventWaitContractEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.(kernelteam.Store).CreateTeamDefinition(t.Context(), conflicting); err != nil {
			t.Fatal(err)
		}
		if err := store.(SkillReferenceUpgradeStore).ApplySkillReferenceUpgrade(t.Context(), wrapped.mutation); !errors.Is(err, ErrSkillReferenceUpgradeConflict) {
			t.Fatalf("conflicting immutable Team definition overwritten: %v", err)
		}
		binding, _ := skill.NewCatalogWithStore(store.(skill.CatalogStore)).GetBinding(t.Context(), skill.ScopeReference(plan.Scope), plan.DeploymentID, plan.BindingID)
		deployment, _ := store.(kernelteam.Store).GetTeamDeployment(t.Context(), capability.ScopeReference(plan.Scope), plan.DeploymentID)
		stored, _ := store.(kernelteam.Store).GetTeamDefinition(t.Context(), conflicting.ID, conflicting.Version)
		if binding.SkillVersion != plan.From.Version || deployment.ActiveVersion != plan.TeamAuthority.DefinitionVersion || !skillUpgradeTeamDefinitionEqual(stored, conflicting) {
			t.Fatalf("immutable collision partially committed: binding %#v deployment %#v stored %#v", binding, deployment, stored)
		}
	})
}

func TestTeamSkillReferenceUpgradeReusesDeterministicImmutableAuthorityAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		service, wrapped, plan := teamSkillUpgradeRuntimeFixture(t, store)
		existing := cloneUpgradeTeamDefinition(wrapped.mutation.TeamAuthority.Definition)
		existing.CreatedAt = existing.CreatedAt.Add(-time.Second)
		if err := store.(kernelteam.Store).CreateTeamDefinition(t.Context(), existing); err != nil {
			t.Fatal(err)
		}
		if err := store.(SkillReferenceUpgradeStore).ApplySkillReferenceUpgrade(t.Context(), wrapped.mutation); err != nil {
			t.Fatal(err)
		}
		stored, _ := store.(kernelteam.Store).GetTeamDefinition(t.Context(), existing.ID, existing.Version)
		if !reflect.DeepEqual(stored, existing) {
			t.Fatalf("reused immutable definition was overwritten: %#v", stored)
		}
		updated, _ := store.(kernelteam.Store).GetTeamDeployment(t.Context(), capability.ScopeReference(plan.Scope), plan.DeploymentID)
		if updated.Revision != 2 || updated.ActiveVersion != existing.Version {
			t.Fatalf("Team activation missing: %#v", updated)
		}
		if len(stored.Roles[1].SkillGrants) != 0 {
			t.Fatal("automatic upgrade granted authority to an ungranted role")
		}
		// Plan does not need a separately supplied Registry to discover canonical
		// Team authority, and replaying the reviewed plan cannot activate it twice.
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Repeat"}); err == nil {
			t.Fatal("stale Team plan replayed")
		}
	})
}

func TestTeamSkillReferenceUpgradeRequiresSharedCanonicalAuthority(t *testing.T) {
	store := NewMemoryStore()
	_, wrapped, plan := teamSkillUpgradeRuntimeFixture(t, store)
	separate := kernelteam.NewMemoryStore()
	previous := wrapped.mutation.TeamAuthority
	if err := separate.CreateTeamDefinition(t.Context(), previous.PreviousDefinition); err != nil {
		t.Fatal(err)
	}
	if err := separate.CreateTeamDeployment(t.Context(), previous.PreviousDeployment, workforce.DefinitionActivation{}); err != nil {
		t.Fatal(err)
	}
	service := NewSkillReferenceUpgradeService(store, skill.NewCatalogWithStore(store), kernelteam.NewRegistryWithStore(separate, nil))
	if _, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: plan.Scope, DeploymentID: plan.DeploymentID, BindingID: plan.BindingID, ToVersion: plan.To.Version}); !errors.Is(err, ErrSkillReferenceUpgradeUnavailable) {
		t.Fatalf("equivalent separate Team store was treated as atomic: %v", err)
	}
}

// A Team whose definition was stored with a digest from an earlier digest
// computation has canonical content and must still be upgradable; the derived
// definition is stored with a freshly computed digest.
func TestTeamSkillReferenceUpgradeAcceptsLegacyDefinitionDigest(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if err := catalog.Register(ctx, &skill.Definition{
			ID: "reader", Version: version, Name: "Reader", Requirements: skill.Requirements{AlwaysAvailable: true},
			Transport: skill.TransportReference{Kind: "tool", Endpoint: "reader"},
			Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read", Risk: skill.RiskLevelRead,
				SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "legacy-team"}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "account", Scope: scope, DeploymentID: "team", SkillID: "reader", SkillVersion: "1.0.0",
		AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	definition, err := kernelteam.PrepareDefinition(&kernelteam.Definition{
		ID: "legacy-design", Version: "1", DisplayName: "Team", Purpose: "Read sources",
		Roles: []kernelteam.RoleSlot{{ID: "reader", DisplayName: "Reader", Purpose: "Read",
			SkillGrants: []kernelteam.RoleSkillGrant{{SkillID: "reader", SkillVersion: "1.0.0", CatalogID: "native-reader", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead}}}},
		Coordination: kernelteam.CoordinationPolicy{QuietByDefault: true, SuppressDuplicateContent: true},
		Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	}, eventWaitContractEpoch)
	if err != nil {
		t.Fatal(err)
	}
	canonicalDigest := definition.Digest
	definition.Digest = "sha256:" + strings.Repeat("0", 64) // recorded by an earlier digest computation
	if err := store.CreateTeamDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	deployment := &kernelteam.Deployment{ID: "team", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		Status: kernelteam.DeploymentActive, Revision: 1, CreatedAt: eventWaitContractEpoch, UpdatedAt: eventWaitContractEpoch}
	if err := store.CreateTeamDeployment(ctx, deployment, workforce.DefinitionActivation{ID: "initial-team", Scope: scope, DeploymentID: deployment.ID,
		DefinitionID: definition.ID, ToVersion: definition.Version, DeploymentRevision: 1, ActorType: "system", ActorID: "fixture", CreatedAt: eventWaitContractEpoch}); err != nil {
		t.Fatal(err)
	}
	service := NewSkillReferenceUpgradeService(store, catalog)
	service.now = func() time.Time { return eventWaitContractEpoch.Add(time.Minute) }
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{Scope: Scope(scope), DeploymentID: deployment.ID, BindingID: "account", ToVersion: "1.1.0"})
	if err != nil || plan.TeamAuthority == nil {
		t.Fatalf("a legacy digest must not hold the Team: %#v %v", plan, err)
	}
	if _, err := service.Apply(ctx, ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Adopt compatible latest Skill"}); err != nil {
		t.Fatal(err)
	}
	upgraded, err := store.GetTeamDeployment(ctx, scope, deployment.ID)
	if err != nil || upgraded.ActiveVersion == definition.Version {
		t.Fatalf("Team did not move to a derived definition: %#v %v", upgraded, err)
	}
	derived, err := store.GetTeamDefinition(ctx, definition.ID, upgraded.ActiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	recomputed, err := kernelteam.PrepareDefinition(derived, derived.CreatedAt)
	if err != nil || recomputed.Digest != derived.Digest || derived.Digest == canonicalDigest {
		t.Fatalf("derived definition digest = %s, recomputed %s, err %v", derived.Digest, recomputed.Digest, err)
	}
}

// Platform bindings on a Team deployment that no role grants upgrade without
// deriving a new Team definition.
func TestTeamSkillReferenceUpgradeOfUngrantedBindingLeavesTeamAuthority(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	for _, id := range []string{"reader", "tools"} {
		for _, version := range []string{"1.0.0", "1.1.0"} {
			if err := catalog.Register(ctx, &skill.Definition{
				ID: id, Version: version, Name: id, Requirements: skill.Requirements{AlwaysAvailable: true},
				Transport: skill.TransportReference{Kind: "tool", Endpoint: id},
				Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read", Risk: skill.RiskLevelRead,
					SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "platform-team"}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "bundled:tools", Scope: scope, DeploymentID: "team", SkillID: "tools", SkillVersion: "1.0.0",
		AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	definition, err := kernelteam.PrepareDefinition(&kernelteam.Definition{
		ID: "platform-design", Version: "1", DisplayName: "Team", Purpose: "Read sources",
		Roles: []kernelteam.RoleSlot{{ID: "reader", DisplayName: "Reader", Purpose: "Read",
			SkillGrants: []kernelteam.RoleSkillGrant{{SkillID: "reader", SkillVersion: "1.0.0", CatalogID: "native-reader", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead}}}},
		Coordination: kernelteam.CoordinationPolicy{QuietByDefault: true, SuppressDuplicateContent: true},
		Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	}, eventWaitContractEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTeamDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	deployment := &kernelteam.Deployment{ID: "team", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		Status: kernelteam.DeploymentActive, Revision: 1, CreatedAt: eventWaitContractEpoch, UpdatedAt: eventWaitContractEpoch}
	if err := store.CreateTeamDeployment(ctx, deployment, workforce.DefinitionActivation{ID: "initial-team", Scope: scope, DeploymentID: deployment.ID,
		DefinitionID: definition.ID, ToVersion: definition.Version, DeploymentRevision: 1, ActorType: "system", ActorID: "fixture", CreatedAt: eventWaitContractEpoch}); err != nil {
		t.Fatal(err)
	}
	service := NewSkillReferenceUpgradeService(store, catalog)
	request := PlanSkillReferenceUpgradeRequest{Scope: Scope(scope), DeploymentID: deployment.ID, BindingID: "bundled:tools", ToVersion: "1.1.0"}
	if _, err := service.Plan(ctx, request); !errors.Is(err, ErrSkillReferenceUpgradeInvalid) {
		t.Fatalf("without the host's claim an ungranted Team binding is a revoked grant: %v", err)
	}
	request.HostManaged = true
	encoded, _ := json.Marshal(request)
	var decoded PlanSkillReferenceUpgradeRequest
	if json.Unmarshal([]byte(strings.Replace(string(encoded), "}", `,"hostManaged":true,"HostManaged":true}`, 1)), &decoded) != nil || decoded.HostManaged || strings.Contains(string(encoded), "ostManaged") {
		t.Fatalf("host management must not travel through JSON: %s %#v", encoded, decoded)
	}
	plan, err := service.Plan(ctx, request)
	if err != nil || plan.TeamAuthority != nil {
		t.Fatalf("ungranted binding plan = %#v %v", plan, err)
	}
	if _, err := service.Apply(ctx, ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Adopt tools 1.1.0"}); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetTeamDeployment(ctx, scope, deployment.ID)
	if err != nil || current.ActiveVersion != definition.Version || current.Revision != deployment.Revision {
		t.Fatalf("Team authority changed for an ungranted binding: %#v %v", current, err)
	}
}
