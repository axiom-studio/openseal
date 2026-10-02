package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestActionRuntimeMaintenanceSerializesGateAndSubmission(t *testing.T) {
	for _, ordering := range []string{"proposal_first", "gate_first", "concurrent"} {
		t.Run(ordering, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
				fixture := newActionRuntimeMaintenanceFixture(t, kernel)
				maintenance := kernel.(SkillRuntimeMaintenanceStore)
				before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
				var result *ActionProposalResult
				var gate *SkillRuntimeMaintenance
				var proposalErr, gateErr error
				propose := func() { result, proposalErr = kernel.CreateActionProposal(t.Context(), fixture.proposal) }
				acquire := func() {
					gate, gateErr = maintenance.AcquireSkillRuntimeMaintenance(t.Context(), actionRuntimeMaintenanceRequest(t, fixture))
				}
				switch ordering {
				case "proposal_first":
					propose()
					acquire()
				case "gate_first":
					acquire()
					propose()
				case "concurrent":
					start := make(chan struct{})
					var workers sync.WaitGroup
					workers.Add(2)
					go func() { defer workers.Done(); <-start; propose() }()
					go func() { defer workers.Done(); <-start; acquire() }()
					close(start)
					workers.Wait()
				}
				if proposalErr == nil {
					if result == nil || !result.Created || gate != nil || !errors.Is(gateErr, ErrSkillReferenceUpgradeBusy) {
						t.Fatalf("maintenance crossed a committed proposal: result=%#v gate=%#v proposalErr=%v gateErr=%v", result, gate, proposalErr, gateErr)
					}
					stored, err := maintenance.GetSkillRuntimeMaintenance(t.Context(), before.Scope, fixture.binding.SkillID)
					if stored != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceNotFound) {
						t.Fatalf("busy acquisition left a gate behind: %#v, %v", stored, err)
					}
				} else {
					if gateErr != nil || gate == nil || !gate.Active || result != nil {
						t.Fatalf("gate and submission did not serialize: result=%#v gate=%#v proposalErr=%v gateErr=%v", result, gate, proposalErr, gateErr)
					}
					assertActionRuntimeMaintenanceError(t, proposalErr, before.Scope, fixture.binding.SkillID)
					assertActionRuntimeMaintenanceNoSubmission(t, kernel, fixture.proposal.Call, before)
				}
			})
		})
	}
}

func TestActionRuntimeMaintenanceCoversLogicalSkillAndPreservesTenantIsolation(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionRuntimeMaintenanceFixture(t, kernel)
		maintenance := kernel.(SkillRuntimeMaintenanceStore)
		otherVersion := actionRuntimeMaintenanceBinding(t, fixture, "other-version", fixture.binding.SkillID, "1.1.0", fixture.binding.SourceIdentity)
		unrelated := actionRuntimeMaintenanceBinding(t, fixture, "unrelated-skill", "unrelated-release", "1.0.0", fixture.binding.SourceIdentity)
		otherSource := actionRuntimeMaintenanceBinding(t, fixture, "other-source", fixture.binding.SkillID, "1.0.0", "native::another-release-publisher")
		// A dormant publisher is outside the enabled canonical snapshot, but
		// cannot escape the tenant's logical-Skill gate through its source pin.
		otherSource.Disabled, otherSource.Revision = true, otherSource.Revision+1
		if err := fixture.catalog.Bind(t.Context(), otherSource); err != nil {
			t.Fatal(err)
		}
		gate, err := maintenance.AcquireSkillRuntimeMaintenance(t.Context(), actionRuntimeMaintenanceRequest(t, fixture))
		if err != nil || gate == nil || !gate.Active {
			t.Fatalf("acquire = %#v, %v", gate, err)
		}
		before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		for _, binding := range []*skill.Binding{fixture.binding, otherVersion, otherSource} {
			proposal := actionRuntimeMaintenanceProposal(fixture.proposal, binding)
			result, err := kernel.CreateActionProposal(t.Context(), proposal)
			if result != nil {
				t.Fatalf("gated logical Skill created a proposal: %#v", result)
			}
			assertActionRuntimeMaintenanceError(t, err, before.Scope, fixture.binding.SkillID)
			assertActionRuntimeMaintenanceNoSubmission(t, kernel, fixture.proposal.Call, before)
		}
		otherTenant := newActionRuntimeMaintenanceTenantFixture(t, fixture)
		otherResult, err := kernel.CreateActionProposal(t.Context(), otherTenant.proposal)
		if err != nil || otherResult == nil || !otherResult.Created {
			t.Fatalf("one tenant's gate blocked another tenant: %#v, %v", otherResult, err)
		}
		unrelatedResult, err := kernel.CreateActionProposal(t.Context(), actionRuntimeMaintenanceProposal(fixture.proposal, unrelated))
		if err != nil || unrelatedResult == nil || !unrelatedResult.Created || unrelatedResult.Call.SkillID != unrelated.SkillID {
			t.Fatalf("one logical Skill's gate blocked an unrelated Skill: %#v, %v", unrelatedResult, err)
		}
	})
}

func TestActionRuntimeMaintenanceOwnerExpiryDoesNotReopenSubmissionOrClaim(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionRuntimeMaintenanceFixture(t, kernel)
		maintenance := kernel.(SkillRuntimeMaintenanceStore)
		request := actionRuntimeMaintenanceRequest(t, fixture)
		request.Now = fixture.now.Add(-10 * time.Minute)
		request.LeaseDuration = time.Minute
		gate, err := maintenance.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil || gate == nil || !gate.Active || !gate.LeaseExpiresAt.Before(fixture.now) {
			t.Fatalf("acquire expired owner fixture = %#v, %v", gate, err)
		}
		before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		result, err := kernel.CreateActionProposal(t.Context(), fixture.proposal)
		if result != nil {
			t.Fatalf("expired owner reopened submissions: %#v", result)
		}
		assertActionRuntimeMaintenanceError(t, err, before.Scope, fixture.binding.SkillID)
		assertActionRuntimeMaintenanceNoSubmission(t, kernel, fixture.proposal.Call, before)
		seedActionRuntimeMaintenanceLegacyCall(t, kernel, fixture)
		claimed, err := kernel.ClaimNextAction(t.Context(), ActionClaim{Scope: before.Scope, WorkerID: "action-worker", Now: fixture.now.Add(2 * time.Second), LeaseDuration: time.Minute})
		if err != nil || claimed != nil {
			t.Fatalf("expired owner reopened execution: %#v, %v", claimed, err)
		}
		stored, err := maintenance.GetSkillRuntimeMaintenance(t.Context(), before.Scope, fixture.binding.SkillID)
		if err != nil || stored == nil || !stored.Active || stored.Revision != gate.Revision {
			t.Fatalf("expired owner silently released its durable gate: %#v, %v", stored, err)
		}
	})
}

func TestActionRuntimeMaintenanceStableGateSkipsLegacyReadyCallBeforeAuthority(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionRuntimeMaintenanceFixture(t, kernel)
		gate, err := kernel.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), actionRuntimeMaintenanceRequest(t, fixture))
		if err != nil || gate == nil || !gate.Active {
			t.Fatalf("acquire = %#v, %v", gate, err)
		}
		seedActionRuntimeMaintenanceLegacyCall(t, kernel, fixture)
		beforeCall, err := kernel.GetActionCall(t.Context(), fixture.proposal.Call.Scope, fixture.proposal.Call.ID)
		if err != nil {
			t.Fatal(err)
		}
		beforeRun := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		catalogCalls, credentialCalls, dispatches := 0, 0, 0
		worker := actionRuntimeMaintenanceWorker(kernel, fixture, &catalogCalls, &credentialCalls, &dispatches)
		for attempt := 0; attempt < 3; attempt++ {
			result, err := worker.RunOnce(t.Context(), beforeRun.Scope, "action-worker", time.Minute)
			if err != nil || result != nil {
				t.Fatalf("stable gate claimed a legacy call: %#v, %v", result, err)
			}
		}
		afterCall, err := kernel.GetActionCall(t.Context(), beforeCall.Scope, beforeCall.ID)
		if err != nil || !reflect.DeepEqual(beforeCall, afterCall) || !reflect.DeepEqual(beforeRun, getActionLifecycleRun(t, kernel, fixture.proposal.Call)) {
			t.Fatalf("stable gate consumed a call attempt or changed its Run: %#v, %v", afterCall, err)
		}
		if catalogCalls != 0 || credentialCalls != 0 || dispatches != 0 {
			t.Fatalf("stable gate reached provider authority: catalog=%d credentials=%d dispatch=%d", catalogCalls, credentialCalls, dispatches)
		}
	})
}

func TestActionRuntimeMaintenanceCoordinatorRejectsBeforeCatalogAndPolicy(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionRuntimeMaintenanceFixture(t, kernel)
		gate, err := kernel.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), actionRuntimeMaintenanceRequest(t, fixture))
		if err != nil || gate == nil || !gate.Active {
			t.Fatalf("acquire = %#v, %v", gate, err)
		}
		before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		catalogCalls, policyCalls := 0, 0
		coordinator := NewActionCoordinator(kernel, kernel, &actionLifecycleCountingCatalog{ActionExecutionCatalog: fixture.catalog, calls: &catalogCalls}, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
			policyCalls++
			return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
		}))
		coordinator.now = func() time.Time { return fixture.now.Add(time.Second) }
		result, err := coordinator.Propose(t.Context(), ProposeActionRequest{
			Scope: before.Scope, RunID: before.ID, WorkerID: "agent-worker", DeploymentID: fixture.binding.DeploymentID,
			BindingID: fixture.binding.ID, BindingRevision: fixture.binding.Revision, SkillID: fixture.binding.SkillID, SkillVersion: fixture.binding.SkillVersion,
			Action: "deploy", Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: fixture.proposal.Call.IdempotencyKey,
		})
		if result != nil || catalogCalls != 0 || policyCalls != 0 {
			t.Fatalf("coordinator reached gated executable authority: result=%#v catalog=%d policy=%d", result, catalogCalls, policyCalls)
		}
		assertActionRuntimeMaintenanceError(t, err, before.Scope, fixture.binding.SkillID)
		assertActionRuntimeMaintenanceNoSubmission(t, kernel, fixture.proposal.Call, before)
	})
}

func TestActionRuntimeMaintenanceObservedAfterClaimDefersWithoutFailure(t *testing.T) {
	for _, boundary := range []string{"after_claim", "after_catalog"} {
		t.Run(boundary, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
				fixture := newActionRuntimeMaintenanceFixture(t, kernel)
				fixture.proposal.Call.MaxAttempts = 1
				if _, err := kernel.CreateActionProposal(t.Context(), fixture.proposal); err != nil {
					t.Fatal(err)
				}
				before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
				request := actionRuntimeMaintenanceRequest(t, fixture)
				gate, _, err := nextSkillRuntimeMaintenance(nil, request)
				if err != nil {
					t.Fatal(err)
				}
				wrapped := &actionRuntimeMaintenanceObservationStore{KernelStore: kernel, maintenance: kernel.(SkillRuntimeMaintenanceStore), gate: gate, active: boundary == "after_claim"}
				catalogCalls, credentialCalls, dispatches := 0, 0, 0
				catalog := &actionRuntimeMaintenanceCatalog{ActionExecutionCatalog: fixture.catalog, calls: &catalogCalls}
				if boundary == "after_catalog" {
					catalog.afterResolve = func() { wrapped.active = true }
				}
				worker := NewActionWorker(wrapped, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
					credentialCalls++
					return map[string]string{"token": "secret"}, nil
				}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
					dispatches++
					return map[string]interface{}{"providerResult": "delivered"}, nil
				}))
				worker.now = func() time.Time { return fixture.now.Add(2 * time.Second) }
				deferred, err := worker.RunOnce(t.Context(), before.Scope, "action-worker", time.Minute)
				if err != nil || deferred == nil || deferred.Call.Status != ActionCallStatusReady || deferred.Call.Attempt != 1 || deferred.Call.CompletedAt != nil || deferred.Call.Output != nil || deferred.Call.LeaseOwner != "" || deferred.Call.LeaseExpiresAt != nil {
					t.Fatalf("maintenance observation became a failure or false result: %#v, %v", deferred, err)
				}
				if dispatches != 0 || boundary == "after_claim" && (catalogCalls != 0 || credentialCalls != 0) {
					t.Fatalf("maintenance boundary reached execution: catalog=%d credentials=%d dispatch=%d", catalogCalls, credentialCalls, dispatches)
				}
				if deferred.Event == nil || deferred.Event.EventType != "action.deferred" || deferred.Event.UsageDelta != nil || !reflect.DeepEqual(before, getActionLifecycleRun(t, kernel, fixture.proposal.Call)) {
					t.Fatalf("maintenance deferral charged usage or changed its Run: %#v", deferred)
				}
			})
		})
	}
}

func TestActionRuntimeMaintenanceVerifiedReleaseUsesFreshVersion(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionRuntimeMaintenanceFixture(t, kernel)
		maintenance := kernel.(SkillRuntimeMaintenanceStore)
		gate, err := maintenance.AcquireSkillRuntimeMaintenance(t.Context(), actionRuntimeMaintenanceRequest(t, fixture))
		if err != nil {
			t.Fatal(err)
		}
		ctx := WithSkillRuntimeMaintenance(t.Context(), gate)
		if err := fixture.store.ApplySkillReferenceUpgrade(ctx, actionBindingFenceUpgrade(fixture)); err != nil {
			t.Fatal(err)
		}
		released, err := maintenance.CompleteSkillRuntimeMaintenance(ctx, SkillRuntimeMaintenanceCompletion{
			Scope: gate.Scope, SkillID: gate.SkillID, OperationID: gate.OperationID, Owner: gate.Owner,
			ExpectedRevision: gate.Revision, VerifiedVersion: "1.1.0", Now: fixture.now.Add(2 * time.Second),
		})
		if err != nil || released == nil || released.Active || released.VerifiedVersion != "1.1.0" {
			t.Fatalf("verified cutover did not release the gate: %#v, %v", released, err)
		}
		binding, err := fixture.catalog.GetBinding(t.Context(), fixture.binding.Scope, fixture.binding.DeploymentID, fixture.binding.ID)
		if err != nil {
			t.Fatal(err)
		}
		proposal := actionRuntimeMaintenanceProposal(fixture.proposal, binding)
		created, err := kernel.CreateActionProposal(t.Context(), proposal)
		if err != nil || created == nil || !created.Created || created.Call.SkillVersion != "1.1.0" {
			t.Fatalf("released gate did not permit a fresh-version action: %#v, %v", created, err)
		}
		dispatches := 0
		worker := NewActionWorker(kernel, fixture.catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			if input.Bound.Definition.Version != "1.1.0" || input.Bound.Binding.SourceIdentity != gate.SourceIdentity || input.Call.BindingRevision != binding.Revision {
				t.Fatalf("released action used stale executable authority: %#v", input)
			}
			return map[string]interface{}{"providerResult": "delivered"}, nil
		}))
		worker.now = func() time.Time { return fixture.now.Add(3 * time.Second) }
		completed, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute)
		if err != nil || completed == nil || completed.Call.Status != ActionCallStatusSucceeded || dispatches != 1 {
			t.Fatalf("fresh-version dispatch failed: %#v, %v, dispatches=%d", completed, err, dispatches)
		}
	})
}

func TestActionRuntimeMaintenancePreservesExistingReceiptReplay(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionRuntimeMaintenanceFixture(t, kernel)
		completeActionBindingFenceFixture(t, fixture)
		maintenance := kernel.(SkillRuntimeMaintenanceStore)
		gate, err := maintenance.AcquireSkillRuntimeMaintenance(t.Context(), actionRuntimeMaintenanceRequest(t, fixture))
		if err != nil || gate == nil || !gate.Active {
			t.Fatalf("acquire after terminal receipt = %#v, %v", gate, err)
		}
		before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		baseline, err := kernel.ListActivity(t.Context(), ActivityFilter{Scope: before.Scope, RunID: before.ID})
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := kernel.CreateActionProposal(t.Context(), fixture.proposal)
		if err != nil || replayed == nil || replayed.Created || replayed.Call.Status != ActionCallStatusSucceeded || replayed.Call.ID != fixture.proposal.Call.ID {
			t.Fatalf("active gate rejected an existing immutable receipt: %#v, %v", replayed, err)
		}
		after := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		events, err := kernel.ListActivity(t.Context(), ActivityFilter{Scope: before.Scope, RunID: before.ID})
		if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(baseline, events) {
			t.Fatalf("gated replay mutated Run or activity: parent=%#v events=%#v, %v", after, events, err)
		}
	})
}

func newActionRuntimeMaintenanceFixture(t *testing.T, kernel KernelStore) *actionBindingFenceFixture {
	t.Helper()
	fixture := newActionBindingFenceFixture(t, kernel)
	if _, ok := kernel.(SkillRuntimeMaintenanceStore); !ok {
		t.Fatal("maintenance fixture requires a durable maintenance store")
	}
	base, _ := governedActionCatalog(t)
	definition, err := base.GetDefinition(t.Context(), fixture.binding.SkillID, fixture.binding.SkillVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "1.1.0"} {
		native := *definition
		native.Version = version
		native.Source = &skill.SourceProvenance{Identity: "native::release", Format: "openseal.skill.v1", Publisher: "maintenance-test"}
		if err := fixture.catalog.Register(t.Context(), &native); err != nil {
			t.Fatal(err)
		}
	}
	fixture.now = time.Now().UTC().Round(0)
	binding := cloneUpgradeBinding(fixture.binding)
	binding.SourceIdentity = "native::release"
	binding.Revision++
	binding.UpdatedAt = fixture.now
	if err := fixture.catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	fixture.binding = binding
	current := mutateActionLifecycleRun(t, kernel, fixture.proposal.Call, fixture.now, func(run *AgentRun) {
		expires := fixture.now.Add(time.Minute)
		run.LeaseExpiresAt = &expires
	})
	updated := cloneAgentRun(current)
	updated.Revision++
	updated.Status = AgentRunStatusWaitingForDependency
	updated.WakeCondition = &WakeCondition{Type: "action", Reference: fixture.proposal.Call.ID}
	updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	updated.UpdatedAt = fixture.now.Add(time.Second)
	fixture.proposal.Run = updated
	fixture.proposal.ExpectedRunRevision = current.Revision
	fixture.proposal.Lease.Now = fixture.now.Add(time.Second)
	fixture.proposal.Call.CreatedAt, fixture.proposal.Call.UpdatedAt = fixture.now.Add(time.Second), fixture.now.Add(time.Second)
	fixture.proposal.Call.AvailableAt = fixture.now.Add(time.Second)
	fixture.proposal.Call.BindingRevision = binding.Revision
	fixture.proposal.Call.InvocationDigest = ComputeActionInvocationDigest(fixture.proposal.Call)
	fixture.proposal.Call.SemanticDigest = ComputeActionSemanticDigest(fixture.proposal.Call)
	fixture.proposal.Event.CreatedAt = fixture.now.Add(time.Second)
	return fixture
}

func newActionRuntimeMaintenanceTenantFixture(t *testing.T, original *actionBindingFenceFixture) *actionBindingFenceFixture {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "maintenance-isolation-" + uuid.NewString()}
	catalog := skill.NewCatalogWithStore(original.store)
	binding := cloneUpgradeBinding(original.binding)
	binding.Scope, binding.Revision = skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, 1
	if err := catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	run := newActionBindingFenceRunningRun(t, original.store, scope, original.now)
	call := cloneActionCall(original.proposal.Call)
	call.ID, call.Scope, call.RunID, call.BindingRevision = uuid.NewString(), scope, run.ID, binding.Revision
	call.InvocationDigest, call.SemanticDigest = ComputeActionInvocationDigest(call), ComputeActionSemanticDigest(call)
	var err error
	call.ExternalOperationDigest, err = computeExternalOperationDigest(scope, run.Owner, run.ObjectiveID, &ExternalOperationIdentity{Resource: "production-release", Operation: "deploy"}, call.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	updated := cloneAgentRun(run)
	updated.Revision++
	updated.Status, updated.WakeCondition = AgentRunStatusWaitingForDependency, &WakeCondition{Type: "action", Reference: call.ID}
	updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	updated.UpdatedAt = original.now.Add(time.Second)
	return &actionBindingFenceFixture{store: original.store, catalog: catalog, binding: binding, now: original.now, proposal: ActionProposalRecord{
		Call: call, Run: updated, ExpectedRunRevision: run.Revision, RequireBindingFence: true,
		Lease: &AgentRunLeaseGuard{WorkerID: "agent-worker", Now: original.now.Add(time.Second)},
		Event: actionLifecycleEvent(call, uuid.NewString(), "action.proposed", original.now.Add(time.Second)),
	}}
}

func actionRuntimeMaintenanceRequest(t *testing.T, fixture *actionBindingFenceFixture) SkillRuntimeMaintenanceRequest {
	t.Helper()
	bindings, err := fixture.store.(SkillRuntimeMaintenanceStore).ListSkillRuntimeMaintenanceBindings(t.Context(), fixture.proposal.Call.Scope, fixture.binding.SkillID, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]SkillRuntimeMaintenanceBindingRevision, len(bindings))
	for i, binding := range bindings {
		expected[i] = SkillRuntimeMaintenanceBindingRevision{DeploymentID: binding.DeploymentID, BindingID: binding.ID, Revision: binding.Revision}
	}
	return SkillRuntimeMaintenanceRequest{
		Scope: fixture.proposal.Call.Scope, SkillID: fixture.binding.SkillID, SourceIdentity: fixture.binding.SourceIdentity,
		RuntimeIdentity: "native-release-server", OperationID: uuid.NewString(), FromVersion: "1.0.0", ToVersion: "1.1.0",
		TargetSourceDigest: "sha256:" + strings.Repeat("c", 64),
		FromSourceDigest:   "sha256:" + strings.Repeat("d", 64), ExpectedBindings: expected,
		FromArtifact: "example.invalid/release@sha256:" + strings.Repeat("a", 64), ToArtifact: "example.invalid/release@sha256:" + strings.Repeat("b", 64),
		Owner: "maintenance-controller", LeaseDuration: 5 * time.Minute, Now: fixture.now,
	}
}

func actionRuntimeMaintenanceBinding(t *testing.T, fixture *actionBindingFenceFixture, id, skillID, version, source string, owned ...context.Context) *skill.Binding {
	t.Helper()
	ctx := t.Context()
	if len(owned) != 0 {
		ctx = owned[0]
	}
	existing, err := fixture.catalog.GetDefinitionVariant(t.Context(), skillID, version, source)
	if err != nil {
		t.Fatal(err)
	}
	if existing == nil {
		definition, err := fixture.catalog.GetDefinitionVariant(t.Context(), fixture.binding.SkillID, "1.0.0", fixture.binding.SourceIdentity)
		if err != nil {
			t.Fatal(err)
		}
		definition.ID, definition.Version = skillID, version
		definition.Source = &skill.SourceProvenance{Identity: source, Format: "openseal.skill.v1", Publisher: "maintenance-test"}
		if err := fixture.catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	binding := cloneUpgradeBinding(fixture.binding)
	binding.ID, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, binding.Revision = id, skillID, version, source, 1
	if err := fixture.catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	return binding
}

func actionRuntimeMaintenanceProposal(original ActionProposalRecord, binding *skill.Binding) ActionProposalRecord {
	proposal := original
	proposal.Call = cloneActionCall(original.Call)
	proposal.Call.BindingID, proposal.Call.BindingRevision = binding.ID, binding.Revision
	proposal.Call.SkillID, proposal.Call.SkillVersion = binding.SkillID, binding.SkillVersion
	proposal.Call.InvocationDigest = ComputeActionInvocationDigest(proposal.Call)
	proposal.Call.SemanticDigest = ComputeActionSemanticDigest(proposal.Call)
	return proposal
}

func assertActionRuntimeMaintenanceError(t *testing.T, err error, scope Scope, skillID string) {
	t.Helper()
	var typed *SkillRuntimeMaintenanceError
	if !errors.Is(err, ErrSkillRuntimeMaintenance) || !errors.As(err, &typed) || typed.Maintenance.Scope != scope || typed.Maintenance.SkillID != skillID || !typed.Maintenance.Active {
		t.Fatalf("missing typed maintenance authority: %v", err)
	}
}

func assertActionRuntimeMaintenanceNoSubmission(t *testing.T, kernel KernelStore, call *ActionCall, before *AgentRun) {
	t.Helper()
	if after := getActionLifecycleRun(t, kernel, call); !reflect.DeepEqual(before, after) {
		t.Fatalf("gated submission changed its parent: before=%#v after=%#v", before, after)
	}
	calls, err := kernel.ListActionCalls(t.Context(), ActionFilter{Scope: call.Scope, RunID: call.RunID})
	if err != nil || len(calls) != 0 {
		t.Fatalf("gated submission inserted an action: %#v, %v", calls, err)
	}
	events, err := kernel.ListActivity(t.Context(), ActivityFilter{Scope: call.Scope, RunID: call.RunID})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType == "action.proposed" || event.Payload["actionCallId"] == call.ID && event.EventType != "run.updated" {
			t.Fatalf("gated submission appended action activity: %#v", event)
		}
	}
}

func actionRuntimeMaintenanceWorker(kernel KernelStore, fixture *actionBindingFenceFixture, catalogCalls, credentialCalls, dispatches *int) *ActionWorker {
	worker := NewActionWorker(kernel, &actionLifecycleCountingCatalog{ActionExecutionCatalog: fixture.catalog, calls: catalogCalls}, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		(*credentialCalls)++
		return map[string]string{"token": "secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		(*dispatches)++
		return map[string]interface{}{"providerResult": "delivered"}, nil
	}))
	worker.now = func() time.Time { return fixture.now.Add(2 * time.Second) }
	return worker
}

// Seed a historical row through trusted backend fixtures. Supported proposal
// APIs cannot create this state because the active gate fences insertion.
func seedActionRuntimeMaintenanceLegacyCall(t *testing.T, kernel KernelStore, fixture *actionBindingFenceFixture) {
	t.Helper()
	transitionActionLifecycleRun(t, kernel, fixture.proposal.Call, AgentRunStatusWaitingForDependency, &WakeCondition{Type: "action", Reference: fixture.proposal.Call.ID}, fixture.now)
	call := fixture.proposal.Call
	payload, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	args := []interface{}{call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.TurnID, call.Status, call.IdempotencyKey,
		externalOperationClaimDigest(call), call.ApprovalID, call.AvailableAt, call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(payload),
		call.DeploymentID, call.BindingID, call.BindingRevision, call.SkillID, call.SkillVersion}
	columns := `(id,scope_kind,scope_id,run_id,turn_id,status,idempotency_key,external_operation_digest,approval_id,available_at,lease_owner,lease_expires_at,revision,created_at,payload,deployment_id,binding_id,binding_revision,skill_id,skill_version)`
	switch store := kernel.(type) {
	case *MemoryStore:
		store.mu.Lock()
		defer store.mu.Unlock()
		store.saveMemoryActionCallLocked(portfolioKey(call.Scope, call.ID), cloneActionCall(call))
		store.actionKeys[actionIdempotencyKey(call.Scope, call.RunID, call.IdempotencyKey)] = call.ID
		store.externalOperationKeys[externalOperationStoreKey(call.Scope, call.ExternalOperationDigest)] = call.ID
	case *SQLiteStore:
		tx, beginErr := store.db.BeginTx(t.Context(), nil)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(t.Context(), "UPDATE skill_runtime_maintenance SET active=0 WHERE scope_kind=? AND scope_id=? AND skill_id=?", call.Scope.Kind, call.Scope.ID, call.SkillID); err == nil {
			_, err = tx.ExecContext(t.Context(), "INSERT INTO action_calls "+columns+" VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+")", args...)
		}
		if err == nil {
			_, err = tx.ExecContext(t.Context(), "UPDATE skill_runtime_maintenance SET active=1 WHERE scope_kind=? AND scope_id=? AND skill_id=?", call.Scope.Kind, call.Scope.ID, call.SkillID)
		}
		if err == nil {
			err = tx.Commit()
		}
	case *PostgresStore:
		placeholders := make([]string, len(args))
		for i := range placeholders {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		tx, beginErr := store.db.BeginTx(t.Context(), nil)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(t.Context(), "UPDATE "+store.table("skill_runtime_maintenance")+" SET active=FALSE WHERE scope_kind=$1 AND scope_id=$2 AND skill_id=$3", call.Scope.Kind, call.Scope.ID, call.SkillID); err == nil {
			_, err = tx.ExecContext(t.Context(), "INSERT INTO "+store.table("action_calls")+" "+columns+" VALUES ("+strings.Join(placeholders, ",")+")", args...)
		}
		if err == nil {
			_, err = tx.ExecContext(t.Context(), "UPDATE "+store.table("skill_runtime_maintenance")+" SET active=TRUE WHERE scope_kind=$1 AND scope_id=$2 AND skill_id=$3", call.Scope.Kind, call.Scope.ID, call.SkillID)
		}
		if err == nil {
			err = tx.Commit()
		}
	default:
		t.Fatal("unsupported legacy action fixture backend")
	}
	if err != nil {
		t.Fatal(err)
	}
}

type actionRuntimeMaintenanceObservationStore struct {
	KernelStore
	maintenance SkillRuntimeMaintenanceStore
	gate        *SkillRuntimeMaintenance
	active      bool
}

func (s *actionRuntimeMaintenanceObservationStore) GetSkillRuntimeMaintenance(ctx context.Context, scope Scope, skillID string) (*SkillRuntimeMaintenance, error) {
	if s.active && scope == s.gate.Scope && skillID == s.gate.SkillID {
		return cloneSkillRuntimeMaintenance(s.gate), nil
	}
	return s.maintenance.GetSkillRuntimeMaintenance(ctx, scope, skillID)
}

type actionRuntimeMaintenanceCatalog struct {
	ActionExecutionCatalog
	calls        *int
	afterResolve func()
}

func (c *actionRuntimeMaintenanceCatalog) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, bindings ...skill.BindingReference) (*skill.BoundAction, error) {
	(*c.calls)++
	bound, err := c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, bindings...)
	if c.afterResolve != nil {
		c.afterResolve()
	}
	return bound, err
}
