package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestActionBindingFenceRejectsMissingStaleDisabledAndForeignAuthority(t *testing.T) {
	for _, invalidAuthority := range []string{"missing", "unqualified", "stale", "disabled", "deleted", "foreign_scope", "foreign_deployment", "foreign_skill", "foreign_version"} {
		t.Run(invalidAuthority, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
				fixture := newActionBindingFenceFixture(t, kernel)
				before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
				proposal := fixture.proposal
				proposal.Call = cloneActionCall(proposal.Call)
				proposal.Run = cloneAgentRun(proposal.Run)
				proposal.Event = cloneActivityEvent(proposal.Event)
				switch invalidAuthority {
				case "missing":
					proposal.Call.BindingID = "missing-binding"
				case "unqualified":
					proposal.Call.BindingID, proposal.Call.BindingRevision = "", 0
				case "stale":
					proposal.Call.BindingRevision++
				case "disabled":
					binding := cloneUpgradeBinding(fixture.binding)
					binding.Disabled = true
					binding.Revision++
					if err := fixture.store.SaveSkillBinding(t.Context(), binding, fixture.binding.Revision); err != nil {
						t.Fatal(err)
					}
					proposal.Call.BindingRevision = binding.Revision
				case "deleted":
					if err := fixture.store.DeleteSkillBinding(t.Context(), fixture.binding.Scope, fixture.binding.DeploymentID, fixture.binding.ID, fixture.binding.Revision); err != nil {
						t.Fatal(err)
					}
				case "foreign_scope":
					proposal.Call.Scope.ID += "-another-tenant"
					proposal.Run.Scope = proposal.Call.Scope
					proposal.Event.Scope = proposal.Call.Scope
				case "foreign_deployment":
					proposal.Call.DeploymentID = "another-agent"
				case "foreign_skill":
					proposal.Call.SkillID = "another-skill"
				case "foreign_version":
					proposal.Call.SkillVersion = "1.1.0"
				}
				proposal.Call.InvocationDigest = ComputeActionInvocationDigest(proposal.Call)
				proposal.Call.SemanticDigest = ComputeActionSemanticDigest(proposal.Call)
				result, err := fixture.store.CreateActionProposal(t.Context(), proposal)
				if result != nil || !errors.Is(err, skill.ErrBindingUnavailable) {
					t.Fatalf("atomic proposal accepted invalid binding authority: %#v, %v", result, err)
				}
				after := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("rejected proposal changed its parent: before=%#v after=%#v", before, after)
				}
				calls, err := fixture.store.ListActionCalls(t.Context(), ActionFilter{Scope: before.Scope, RunID: before.ID})
				if err != nil || len(calls) != 0 {
					t.Fatalf("rejected proposal inserted an action: %#v, %v", calls, err)
				}
				events, err := fixture.store.ListActivity(t.Context(), ActivityFilter{Scope: before.Scope, RunID: before.ID})
				if err != nil || len(events) != 0 {
					t.Fatalf("rejected proposal appended activity: %#v, %v", events, err)
				}
			})
		})
	}
}

func TestActionBindingFenceCoordinatorProtectsOrdinaryAndDuplicateProposals(t *testing.T) {
	for _, path := range []string{"ordinary", "duplicate_receipt"} {
		t.Run(path, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
				fixture := newActionBindingFenceFixture(t, kernel)
				run := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
				if path == "duplicate_receipt" {
					completeActionBindingFenceFixture(t, fixture)
					run = newActionBindingFenceRunningRun(t, fixture.store, fixture.proposal.Call.Scope, fixture.now)
				}
				before := cloneAgentRun(run)
				intercepted := &actionBindingFenceProposalStore{ActionStore: fixture.store}
				intercepted.beforeCreate = func(proposal ActionProposalRecord) {
					if !proposal.RequireBindingFence {
						t.Fatal("durable coordinator omitted the atomic binding fence")
					}
					if (proposal.Call.DuplicateOfActionCallID != "") != (path == "duplicate_receipt") {
						t.Fatalf("wrong coordinator path exercised: %#v", proposal.Call)
					}
					binding := cloneUpgradeBinding(fixture.binding)
					binding.Disabled = true
					binding.Revision++
					if err := fixture.store.SaveSkillBinding(t.Context(), binding, fixture.binding.Revision); err != nil {
						t.Fatal(err)
					}
				}
				coordinator := NewActionCoordinator(fixture.store, intercepted, fixture.catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
					return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
				}))
				coordinator.now = func() time.Time { return fixture.now.Add(time.Second) }
				result, err := coordinator.Propose(t.Context(), ProposeActionRequest{
					Scope: run.Scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: fixture.binding.DeploymentID,
					BindingID: fixture.binding.ID, BindingRevision: fixture.binding.Revision,
					SkillID: fixture.binding.SkillID, SkillVersion: fixture.binding.SkillVersion, Action: "deploy",
					Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: fixture.proposal.Call.IdempotencyKey,
					ExternalOperation: &ExternalOperationIdentity{Resource: "production-release", Operation: "deploy"},
				})
				if result != nil || !errors.Is(err, skill.ErrBindingUnavailable) || intercepted.creates != 1 {
					t.Fatalf("coordinator did not fence binding change after catalog resolution: %#v, %v, creates=%d", result, err, intercepted.creates)
				}
				after, err := fixture.store.GetAgentRun(t.Context(), run.Scope, run.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("rejected coordinator proposal changed its parent: %#v, %v", after, err)
				}
				calls, err := fixture.store.ListActionCalls(t.Context(), ActionFilter{Scope: run.Scope, RunID: run.ID})
				if err != nil || len(calls) != 0 {
					t.Fatalf("rejected coordinator proposal inserted an action: %#v, %v", calls, err)
				}
			})
		})
	}
}

func TestActionBindingFencePreservesIdempotentReceiptAfterUpgrade(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionBindingFenceFixture(t, kernel)
		completeActionBindingFenceFixture(t, fixture)
		before := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		baseline, err := fixture.store.ListActivity(t.Context(), ActivityFilter{Scope: before.Scope, RunID: before.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.ApplySkillReferenceUpgrade(t.Context(), actionBindingFenceUpgrade(fixture)); err != nil {
			t.Fatal(err)
		}
		replayed, err := fixture.store.CreateActionProposal(t.Context(), fixture.proposal)
		if err != nil || replayed == nil || replayed.Created || replayed.Call.ID != fixture.proposal.Call.ID || replayed.Call.Status != ActionCallStatusSucceeded || replayed.Call.SkillVersion != "1.0.0" {
			t.Fatalf("binding upgrade lost the immutable idempotent receipt: %#v, %v", replayed, err)
		}
		after := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		events, err := fixture.store.ListActivity(t.Context(), ActivityFilter{Scope: before.Scope, RunID: before.ID})
		if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(baseline, events) {
			t.Fatalf("idempotent replay changed Run or activity: parent=%#v events=%#v, %v", after, events, err)
		}
		bindings, err := fixture.store.ListSkillBindings(t.Context(), fixture.binding.Scope, fixture.binding.DeploymentID)
		if err != nil || len(bindings) != 1 || bindings[0].Revision != 2 || bindings[0].SkillVersion != "1.1.0" {
			t.Fatalf("replay changed upgraded authority: %#v, %v", bindings, err)
		}
	})
}

func TestActionBindingFenceSerializesProposalAndUpgrade(t *testing.T) {
	for _, ordering := range []string{"proposal_first", "upgrade_first", "concurrent"} {
		t.Run(ordering, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
				fixture := newActionBindingFenceFixture(t, kernel)
				mutation := actionBindingFenceUpgrade(fixture)
				var proposed *ActionProposalResult
				var proposalErr, upgradeErr error
				propose := func() { proposed, proposalErr = fixture.store.CreateActionProposal(t.Context(), fixture.proposal) }
				upgrade := func() { upgradeErr = fixture.store.ApplySkillReferenceUpgrade(t.Context(), mutation) }
				switch ordering {
				case "proposal_first":
					propose()
					upgrade()
				case "upgrade_first":
					upgrade()
					propose()
				case "concurrent":
					start := make(chan struct{})
					var workers sync.WaitGroup
					workers.Add(2)
					go func() { defer workers.Done(); <-start; propose() }()
					go func() { defer workers.Done(); <-start; upgrade() }()
					close(start)
					workers.Wait()
				}
				if proposalErr == nil {
					if proposed == nil || !proposed.Created || !errors.Is(upgradeErr, ErrSkillReferenceUpgradeBusy) {
						t.Fatalf("upgrade passed newly committed outstanding work: proposal=%#v, proposalErr=%v, upgradeErr=%v", proposed, proposalErr, upgradeErr)
					}
				} else if upgradeErr != nil || !errors.Is(proposalErr, skill.ErrBindingUnavailable) {
					t.Fatalf("proposal and upgrade did not serialize: proposalErr=%v upgradeErr=%v", proposalErr, upgradeErr)
				}
				bindings, err := fixture.store.ListSkillBindings(t.Context(), fixture.binding.Scope, fixture.binding.DeploymentID)
				if err != nil || len(bindings) != 1 {
					t.Fatalf("get binding = %#v, %v", bindings, err)
				}
				run := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
				calls, err := fixture.store.ListActionCalls(t.Context(), ActionFilter{Scope: run.Scope, RunID: run.ID})
				if err != nil {
					t.Fatal(err)
				}
				if proposalErr == nil {
					if bindings[0].Revision != 1 || len(calls) != 1 || run.Status != AgentRunStatusWaitingForDependency {
						t.Fatalf("successful proposal crossed upgrade: binding=%#v calls=%#v run=%#v", bindings[0], calls, run)
					}
				} else if bindings[0].Revision != 2 || len(calls) != 0 || run.Status != AgentRunStatusRunning {
					t.Fatalf("successful upgrade left a stale proposal: binding=%#v calls=%#v run=%#v", bindings[0], calls, run)
				}
			})
		})
	}
}

func TestActionBindingFenceSerializesSameExternalOperationAcrossRuns(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionBindingFenceFixture(t, kernel)
		firstRun := getActionLifecycleRun(t, kernel, fixture.proposal.Call)
		secondRun := newActionBindingFenceRunningRun(t, fixture.store, firstRun.Scope, fixture.now)
		runs := []*AgentRun{firstRun, secondRun}
		var outcomes [2]*ActionProposalResult
		var failures [2]error
		start := make(chan struct{})
		var proposals sync.WaitGroup
		proposals.Add(2)
		for index, run := range runs {
			go func(index int, run *AgentRun) {
				defer proposals.Done()
				coordinator := NewActionCoordinator(fixture.store, fixture.store, fixture.catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
					return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
				}))
				coordinator.now = func() time.Time { return fixture.now.Add(time.Second) }
				<-start
				outcomes[index], failures[index] = coordinator.Propose(t.Context(), ProposeActionRequest{
					Scope: run.Scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: fixture.binding.DeploymentID,
					BindingID: fixture.binding.ID, BindingRevision: fixture.binding.Revision,
					SkillID: fixture.binding.SkillID, SkillVersion: fixture.binding.SkillVersion, Action: "deploy",
					Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: fixture.proposal.Call.IdempotencyKey,
					ExternalOperation: &ExternalOperationIdentity{Resource: "production-release", Operation: "deploy"},
				})
			}(index, run)
		}
		close(start)
		proposals.Wait()
		var original *ActionCall
		for index, result := range outcomes {
			if failures[index] == nil {
				if result == nil || !result.Created || result.Call.Status != ActionCallStatusReady || result.Call.DuplicateOfActionCallID != "" || original != nil {
					t.Fatalf("same external operation created more than one original: outcomes=%#v failures=%v", outcomes, failures)
				}
				original = result.Call
			} else if result != nil || !errors.Is(failures[index], ErrExternalOperationClaimed) {
				t.Fatalf("competing operation was not durably fenced: result=%#v, error=%v", result, failures[index])
			}
		}
		if original == nil || original.ExternalOperationDigest != fixture.proposal.Call.ExternalOperationDigest {
			t.Fatalf("no original operation receipt: outcomes=%#v failures=%v", outcomes, failures)
		}
		calls, err := fixture.store.ListActionCalls(t.Context(), ActionFilter{Scope: firstRun.Scope})
		if err != nil || len(calls) != 1 || calls[0].ID != original.ID || calls[0].Status != ActionCallStatusReady {
			t.Fatalf("competing operation created another executable action: %#v, %v", calls, err)
		}
		for _, before := range runs {
			after, err := fixture.store.GetAgentRun(t.Context(), before.Scope, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.ID == original.RunID {
				if after.Status != AgentRunStatusWaitingForDependency || after.WakeCondition == nil || after.WakeCondition.Reference != original.ID {
					t.Fatalf("original operation did not own its dependency: %#v", after)
				}
			} else if !reflect.DeepEqual(before, after) {
				t.Fatalf("competing proposal changed the losing Run: before=%#v after=%#v", before, after)
			}
		}
		claimed, err := fixture.store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: firstRun.Scope, WorkerID: "action-worker", Now: fixture.now.Add(2 * time.Second), LeaseDuration: time.Minute,
		})
		if err != nil || claimed == nil || claimed.ID != original.ID {
			t.Fatalf("claim original operation = %#v, %v", claimed, err)
		}
		secondClaim, err := fixture.store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: firstRun.Scope, WorkerID: "another-action-worker", Now: fixture.now.Add(3 * time.Second), LeaseDuration: time.Minute,
		})
		if err != nil || secondClaim != nil {
			t.Fatalf("same external operation had a second execution available: %#v, %v", secondClaim, err)
		}
	})
}

type actionBindingFenceStore interface {
	KernelStore
	skill.CatalogStore
	SkillReferenceUpgradeStore
}

type actionBindingFenceFixture struct {
	store    actionBindingFenceStore
	catalog  *skill.Catalog
	binding  *skill.Binding
	proposal ActionProposalRecord
	now      time.Time
}

func newActionBindingFenceFixture(t *testing.T, kernel KernelStore) *actionBindingFenceFixture {
	t.Helper()
	store, ok := kernel.(actionBindingFenceStore)
	if !ok {
		t.Fatal("binding fence fixture requires a durable catalog and upgrade store")
	}
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	base, originalScope := governedActionCatalog(t)
	definition, err := base.GetDefinition(t.Context(), "release", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	next := *definition
	next.Version = "1.1.0"
	if err := catalog.Register(t.Context(), &next); err != nil {
		t.Fatal(err)
	}
	binding, err := base.GetBinding(t.Context(), skill.ScopeReference{Kind: originalScope.Kind, ID: originalScope.ID}, "release-agent", "release-binding")
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "binding-fence-" + uuid.NewString()}
	binding.Scope = skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	binding.CreatedAt, binding.UpdatedAt = now, now
	if err := catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	binding, err = catalog.GetBinding(t.Context(), binding.Scope, binding.DeploymentID, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := newActionBindingFenceRunningRun(t, store, scope, now)
	call := &ActionCall{
		ID: uuid.NewString(), Scope: scope, RunID: run.ID, DeploymentID: binding.DeploymentID,
		BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: binding.SkillID, SkillVersion: binding.SkillVersion,
		Action: "deploy", Status: ActionCallStatusReady, Risk: skill.RiskLevelProduction, SideEffect: skill.SideEffectExternal,
		Arguments: map[string]interface{}{"environment": "production"}, CredentialRefs: binding.Credentials,
		IdempotencyKey: "deploy-production", MaxAttempts: 3, AvailableAt: now.Add(time.Second), Revision: 1,
		CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	call.ExternalOperationDigest, err = computeExternalOperationDigest(scope, run.Owner, run.ObjectiveID, &ExternalOperationIdentity{Resource: "production-release", Operation: "deploy"}, call.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	updated := cloneAgentRun(run)
	updated.Revision++
	updated.Status = AgentRunStatusWaitingForDependency
	updated.WakeCondition = &WakeCondition{Type: "action", Reference: call.ID}
	updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	updated.UpdatedAt = now.Add(time.Second)
	return &actionBindingFenceFixture{
		store: store, catalog: catalog, binding: binding, now: now,
		proposal: ActionProposalRecord{
			Call: call, Run: updated, ExpectedRunRevision: run.Revision, RequireBindingFence: true,
			Lease: &AgentRunLeaseGuard{WorkerID: "agent-worker", Now: now.Add(time.Second)},
			Event: actionLifecycleEvent(call, uuid.NewString(), "action.proposed", now.Add(time.Second)),
		},
	}
}

func newActionBindingFenceRunningRun(t *testing.T, store KernelStore, scope Scope, now time.Time) *AgentRun {
	t.Helper()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent", Goal: "Deploy safely", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim running fixture = %#v, %v", claimed, err)
	}
	return claimed
}

func completeActionBindingFenceFixture(t *testing.T, fixture *actionBindingFenceFixture) {
	t.Helper()
	if _, err := fixture.store.CreateActionProposal(t.Context(), fixture.proposal); err != nil {
		t.Fatal(err)
	}
	worker := NewActionWorker(fixture.store, fixture.catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"providerResult": "delivered"}, nil
	}))
	worker.now = func() time.Time { return fixture.now.Add(2 * time.Second) }
	completed, err := worker.RunOnce(t.Context(), fixture.proposal.Call.Scope, "action-worker", time.Minute)
	if err != nil || completed == nil || completed.Call.Status != ActionCallStatusSucceeded {
		t.Fatalf("complete fixture = %#v, %v", completed, err)
	}
	transitionActionLifecycleRun(t, fixture.store, fixture.proposal.Call, AgentRunStatusCanceled, nil, fixture.now.Add(3*time.Second))
}

func actionBindingFenceUpgrade(fixture *actionBindingFenceFixture) *SkillReferenceUpgradeMutation {
	next := cloneUpgradeBinding(fixture.binding)
	next.SkillVersion = "1.1.0"
	next.Revision++
	next.UpdatedAt = fixture.now.Add(time.Minute)
	plan := &SkillReferenceUpgradePlan{
		APIVersion: SkillReferenceUpgradeAPIVersion, Scope: fixture.proposal.Call.Scope,
		DeploymentID: next.DeploymentID, BindingID: next.ID, ExpectedBindingRevision: fixture.binding.Revision,
		From:   SkillReferenceIdentity{ID: fixture.binding.SkillID, Version: fixture.binding.SkillVersion, SourceIdentity: fixture.binding.SourceIdentity},
		To:     SkillReferenceIdentity{ID: next.SkillID, Version: next.SkillVersion, SourceIdentity: next.SourceIdentity},
		Digest: "sha256:reviewed-binding-fence",
	}
	return &SkillReferenceUpgradeMutation{
		Plan: plan, Binding: next,
		Receipt: &SkillReferenceUpgradeReceipt{
			APIVersion: SkillReferenceUpgradeAPIVersion, PlanDigest: plan.Digest, Scope: plan.Scope,
			DeploymentID: next.DeploymentID, BindingID: next.ID, BindingRevision: next.Revision,
			From: plan.From, To: plan.To, AppliedAt: next.UpdatedAt,
		},
	}
}

type actionBindingFenceProposalStore struct {
	ActionStore
	beforeCreate func(ActionProposalRecord)
	creates      int
}

func (s *actionBindingFenceProposalStore) CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	s.creates++
	s.beforeCreate(proposal)
	return s.ActionStore.CreateActionProposal(ctx, proposal)
}
