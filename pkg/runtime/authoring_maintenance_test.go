package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestWorkforceMaintenanceFencesAtomicActivation(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store, ok := kernel.(authoring.AtomicChangeSetStore)
		if !ok {
			t.Skip("the standalone memory authoring store does not mutate runtime bindings")
		}
		for _, scenario := range []string{"incoming", "controller-context", "other-skill", "other-tenant"} {
			t.Run(scenario, func(t *testing.T) {
				ctx := t.Context()
				value := workforceMaintenanceCandidate(t, kernel, scenario)
				if _, _, err := store.CreateChangeSet(ctx, value, scenario, scenario); err != nil {
					t.Fatal(err)
				}
				request := workforceMaintenanceRequest(Scope(value.Scope), "research")
				if scenario == "other-skill" {
					request.SkillID = "different"
				}
				if scenario == "other-tenant" {
					request.Scope.ID = "different"
				}
				gate, err := kernel.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "controller-context" {
					ctx = WithSkillRuntimeMaintenance(ctx, gate)
				}
				applied := appliedRuntimeChangeSet(value, "receipt-"+scenario, "apply-"+scenario, value.UpdatedAt.Add(time.Minute))
				result, err := store.ApplyChangeSet(ctx, applied, value.Revision)
				if scenario == "other-skill" || scenario == "other-tenant" {
					if err != nil || result == nil || result.Status != authoring.ChangeSetApplied {
						t.Fatalf("unrelated activation = %#v, %v", result, err)
					}
					return
				}
				assertActionRuntimeMaintenanceError(t, err, Scope(value.Scope), "research")
				stored, err := store.GetChangeSet(ctx, value.Scope, value.ID)
				if err != nil || stored.Status != authoring.ChangeSetReady || stored.Revision != value.Revision || stored.ApplyReceipt != nil {
					t.Fatalf("failed activation changed receipt: %#v, %v", stored, err)
				}
				assertWorkforceMaintenanceNoAggregate(t, kernel, value)
			})
		}
	})
}

func TestWorkforceMaintenanceFencesRemovedGrantAndPreservesAppliedReplay(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store, ok := kernel.(authoring.AtomicChangeSetStore)
		if !ok {
			t.Skip("the standalone memory authoring store does not mutate runtime bindings")
		}
		ctx := t.Context()
		value := workforceMaintenanceCandidate(t, kernel, "remove")
		if _, _, err := store.CreateChangeSet(ctx, value, "create", "create"); err != nil {
			t.Fatal(err)
		}
		applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
		if _, err := store.ApplyChangeSet(ctx, applied, value.Revision); err != nil {
			t.Fatal(err)
		}
		maintenance := kernel.(SkillRuntimeMaintenanceStore)
		request := workforceMaintenanceRequest(Scope(value.Scope), "research")
		bindings, err := maintenance.ListSkillRuntimeMaintenanceBindings(ctx, request.Scope, request.SkillID, "", "", 100)
		if err != nil || len(bindings) != 1 {
			t.Fatalf("initial bindings = %#v, %v", bindings, err)
		}
		request.ExpectedBindings = []SkillRuntimeMaintenanceBindingRevision{{DeploymentID: bindings[0].DeploymentID, BindingID: bindings[0].ID, Revision: bindings[0].Revision}}
		gate, err := maintenance.AcquireSkillRuntimeMaintenance(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if replay, err := store.ApplyChangeSet(ctx, cloneRuntimeChangeSet(applied), value.Revision); err != nil || replay == nil || replay.ApplyReceipt.ID != applied.ApplyReceipt.ID {
			t.Fatalf("existing receipt replay under maintenance = %#v, %v", replay, err)
		}
		amend := cloneRuntimeChangeSet(value)
		amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest = "remove-research", value.ID, authoring.ModeAmend, "remove-research-candidate"
		amend.Result.Candidate.Agents[0].Version = "2"
		amend.Result.Candidate.Agents[0].SkillRequirements = nil
		amend.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = nil
		amend.Result.Candidate.Team.Version = "2"
		amend.Catalog = authoring.CapabilityCatalog{}
		amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
		amend.Placement.TeamExpectedRevision = 1
		for key, placement := range amend.Placement.Objectives {
			placement.ExpectedRevision = 1
			amend.Placement.Objectives[key] = placement
		}
		if _, _, err := store.CreateChangeSet(ctx, amend, "remove", "remove"); err != nil {
			t.Fatal(err)
		}
		removed := appliedRuntimeChangeSet(amend, "receipt-remove", "apply-remove", amend.UpdatedAt.Add(2*time.Minute))
		_, err = store.ApplyChangeSet(WithSkillRuntimeMaintenance(ctx, gate), removed, amend.Revision)
		assertActionRuntimeMaintenanceError(t, err, request.Scope, request.SkillID)
		after, err := maintenance.ListSkillRuntimeMaintenanceBindings(ctx, request.Scope, request.SkillID, "", "", 100)
		if err != nil || len(after) != 1 || after[0].ID != bindings[0].ID || after[0].Revision != bindings[0].Revision {
			t.Fatalf("gated removal changed grant = %#v, %v", after, err)
		}
		deployment, err := kernel.(interface {
			GetDeployment(context.Context, capability.ScopeReference, string) (*agent.AgentDeployment, error)
		}).GetDeployment(ctx, value.Scope, "agent-live-remove")
		if err != nil || deployment.Revision != 1 || deployment.ActiveVersion != "1" {
			t.Fatalf("gated removal changed deployment = %#v, %v", deployment, err)
		}
		if _, err := kernel.(interface {
			GetDefinition(context.Context, string, string) (*agent.AgentDefinition, error)
		}).GetDefinition(ctx, "agent", "2"); !errors.Is(err, agent.ErrDefinitionNotFound) {
			t.Fatalf("gated removal persisted a definition = %v", err)
		}
	})
}

func workforceMaintenanceCandidate(t *testing.T, kernel KernelStore, suffix string) *authoring.ChangeSet {
	t.Helper()
	catalog := skill.NewCatalogWithStore(kernel.(skill.CatalogStore))
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if existing, err := catalog.GetDefinitionVariant(t.Context(), "research", version, "native::research"); err == nil && existing != nil {
			continue
		}
		definition := &skill.Definition{ID: "research", Version: version, Name: "Research", Prompt: &skill.PromptModule{Instructions: "Preserve cited evidence."}, Source: &skill.SourceProvenance{Identity: "native::research", Format: "openseal.skill.v1", Publisher: "test"}}
		if err := catalog.Register(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
	}
	value := testApplicableWorkforceChangeSet()
	value.ID = "change-" + suffix
	value.Scope.ID = "workforce-" + suffix
	value.Placement.AgentDeploymentIDs["agent"] = "agent-live-" + suffix
	value.Placement.TeamDeploymentID = "team-live-" + suffix
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", PromptRequired: true}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{"research": {ID: "research", Version: "1.0.0", SourceIdentity: "native::research", PromptAvailable: true}}}
	return value
}

func workforceMaintenanceRequest(scope Scope, skillID string) SkillRuntimeMaintenanceRequest {
	return SkillRuntimeMaintenanceRequest{Scope: scope, SkillID: skillID, SourceIdentity: "native::research", RuntimeIdentity: "research-server", OperationID: "research-operation", FromVersion: "1.0.0", ToVersion: "1.1.0", FromSourceDigest: "sha256:" + strings.Repeat("a", 64), TargetSourceDigest: "sha256:" + strings.Repeat("b", 64), FromArtifact: "example.invalid/research@sha256:" + strings.Repeat("c", 64), ToArtifact: "example.invalid/research@sha256:" + strings.Repeat("d", 64), Owner: "controller", Now: time.Now().UTC(), LeaseDuration: 5 * time.Minute}
}

func assertWorkforceMaintenanceNoAggregate(t *testing.T, kernel KernelStore, value *authoring.ChangeSet) {
	t.Helper()
	agents := kernel.(interface {
		GetDeployment(context.Context, capability.ScopeReference, string) (*agent.AgentDeployment, error)
	})
	if deployment, err := agents.GetDeployment(t.Context(), value.Scope, value.Placement.AgentDeploymentIDs["agent"]); !errors.Is(err, agent.ErrDeploymentNotFound) {
		t.Fatalf("failed activation persisted deployment = %#v, %v", deployment, err)
	}
	bindings, err := kernel.(skill.CatalogStore).ListSkillBindings(t.Context(), value.Scope, value.Placement.AgentDeploymentIDs["agent"])
	if err != nil || len(bindings) != 0 {
		t.Fatalf("failed activation persisted bindings = %#v, %v", bindings, err)
	}
	objectives, err := kernel.ListObjectives(t.Context(), ObjectiveFilter{Scope: Scope(value.Scope)})
	if err != nil || len(objectives) != 0 {
		t.Fatalf("failed activation persisted objectives = %#v, %v", objectives, err)
	}
}
