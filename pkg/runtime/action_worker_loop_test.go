package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const loopWorkerID = "loop-worker"

// loopActionSpec describes one loop tool call proposeLoopAction stores.
type loopActionSpec struct {
	id, skillID, version, action string
	arguments                    map[string]interface{}
	externalOperation            *ExternalOperationIdentity
}

// proposeLoopAction stores a ready loop call of step-1 for the Run's lease
// owner workerID, as ActionCoordinator.Propose does for a governed loop tool
// call: the Run is unchanged except for the action reservation of a budgeted
// Run.
func proposeLoopAction(t *testing.T, store KernelStore, catalog ActionExecutionCatalog, run *AgentRun, workerID string, now time.Time, spec loopActionSpec) *ActionProposalResult {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: current.Scope.Kind, ID: current.Scope.ID}, current.AssignedAgentID, spec.skillID, spec.version, spec.action)
	if err != nil {
		t.Fatal(err)
	}
	call := &ActionCall{
		ID: spec.id, Scope: current.Scope, RunID: current.ID, TurnID: "step-1", DeploymentID: current.AssignedAgentID,
		BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision,
		SkillID: spec.skillID, SkillVersion: spec.version, Action: spec.action, Origin: ActionOriginLoop, Execution: ActionExecutionLoop,
		Status: ActionCallStatusReady, Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect, Review: bound.Action.Review,
		Arguments: cloneMap(spec.arguments), CredentialRefs: boundCredentialReferences(bound), IdempotencyKey: "tool-call:step-1:" + spec.id,
		MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if spec.externalOperation != nil {
		call.ExternalOperationDigest, err = computeExternalOperationDigest(current.Scope, current.Owner, current.ObjectiveID, spec.externalOperation, call.IdempotencyKey)
		if err != nil {
			t.Fatal(err)
		}
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	record := ActionProposalRecord{
		Call: call, ExpectedRunRevision: current.Revision, Lease: &AgentRunLeaseGuard{WorkerID: workerID, Now: now},
		Event: &ActivityEvent{
			ID: "proposed-" + spec.id, Scope: current.Scope, EventType: "action.proposed", Severity: ActivitySeverityInfo, AgentID: current.AssignedAgentID,
			RunID: current.ID, TurnID: "step-1", Actor: ActivityActor{Type: "worker", ID: workerID}, Summary: "Proposed " + spec.action,
			Visibility: ActivityVisibilityScope, CreatedAt: now, Payload: map[string]interface{}{"actionCallId": spec.id},
		},
	}
	if current.Budget != nil {
		updated := cloneAgentRun(current)
		if err := reserveRunBudget(updated, BudgetReservation{ID: actionBudgetReservationID(call.ID), Usage: BudgetUsage{Actions: 1}, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		updated.Revision++
		updated.UpdatedAt = now
		record.Run = updated
	}
	result, err := store.CreateActionProposal(ctx, record)
	if err != nil || result == nil || !result.Created {
		t.Fatalf("propose loop call %s = %#v, %v", spec.id, result, err)
	}
	return result
}

// loopWorkerCatalog binds release-agent to a Skill with a production write
// (deploy) and a read (status), both using one credential.
func loopWorkerCatalog(t *testing.T, scope Scope) *skill.Catalog {
	t.Helper()
	ctx := context.Background()
	schema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"environment": map[string]interface{}{"type": "string"}}, "required": []interface{}{"environment"},
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "release", Version: "1.0.0", Name: "Release", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://release.invalid"},
		Actions: map[string]skill.Action{
			"deploy": {Name: "deploy", Description: "Deploy a release", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelProduction, InputSchema: schema,
				Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}}, Idempotency: skill.IdempotencyRequired},
			"status": {Name: "status", Description: "Read release status", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, InputSchema: schema,
				Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}}, Idempotency: skill.IdempotencyNone},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "release-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy", "status"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-secret"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// loopWorkerFixture is one running Run of release-agent claimed by
// loopWorkerID for an hour, whose step-1 proposes loop calls.
type loopWorkerFixture struct {
	t       *testing.T
	store   KernelStore
	catalog *skill.Catalog
	scope   Scope
	now     time.Time
	clock   time.Time
	run     *AgentRun

	catalogCalls, credentialCalls, dispatches int
}

func newLoopWorkerFixture(t *testing.T, store KernelStore, budget *BudgetPolicy) *loopWorkerFixture {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "loop-worker"}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	created, err := portfolio.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent",
		Goal: "ship the release", Source: RunSourceObjective, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: scope, WorkerID: loopWorkerID, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	return &loopWorkerFixture{t: t, store: store, catalog: loopWorkerCatalog(t, scope), scope: scope, now: now, clock: now.Add(2 * time.Second), run: run}
}

func (f *loopWorkerFixture) propose(id, action string) *ActionCall {
	f.t.Helper()
	result := proposeLoopAction(f.t, f.store, f.catalog, f.run, loopWorkerID, f.now.Add(time.Second), loopActionSpec{
		id: id, skillID: "release", version: "1.0.0", action: action, arguments: map[string]interface{}{"environment": "production"},
	})
	f.run = f.currentRun()
	return result.Call
}

// worker counts catalog resolutions, credential resolutions and dispatches;
// its clock is f.clock.
func (f *loopWorkerFixture) worker(dispatch ActionDispatcherFunc) *ActionWorker {
	worker := NewActionWorker(f.store, &actionLifecycleCountingCatalog{ActionExecutionCatalog: f.catalog, calls: &f.catalogCalls},
		CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			f.credentialCalls++
			return map[string]string{"token": "loop-secret"}, nil
		}), ActionDispatcherFunc(func(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
			f.dispatches++
			return dispatch(ctx, input)
		}))
	worker.now = func() time.Time { return f.clock }
	return worker
}

func (f *loopWorkerFixture) execute(worker *ActionWorker, id string) (*ActionExecutionResult, error) {
	return worker.ExecuteOwnLoopCall(context.Background(), f.scope, id, loopWorkerID)
}

func (f *loopWorkerFixture) currentRun() *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(context.Background(), f.scope, f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("get Run = %#v, %v", run, err)
	}
	return run
}

func (f *loopWorkerFixture) call(id string) *ActionCall {
	f.t.Helper()
	call, err := f.store.GetActionCall(context.Background(), f.scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return call
}

// setRunStatus moves the Run to status outside its lease, as a park, pause
// or cancel does.
func (f *loopWorkerFixture) setRunStatus(status AgentRunStatus) {
	f.t.Helper()
	current := f.currentRun()
	updated := cloneAgentRun(current)
	updated.Status = status
	updated.Revision++
	updated.UpdatedAt = f.clock
	if status != AgentRunStatusRunning {
		updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	}
	if isTerminalAgentRunStatus(status) {
		completed := f.clock
		updated.CompletedAt = &completed
	}
	event := actionLifecycleEvent(&ActionCall{Scope: f.scope, RunID: f.run.ID, DeploymentID: "release-agent"}, fmt.Sprintf("status-%s-%d", status, updated.Revision), "run.status_changed", f.clock)
	if _, err := f.store.UpdateAgentRunWithEvent(context.Background(), updated, current.Revision, event, nil); err != nil {
		f.t.Fatalf("set Run %s: %v", status, err)
	}
	f.run = f.currentRun()
}

func (f *loopWorkerFixture) requireNoAuthority() {
	f.t.Helper()
	if f.catalogCalls != 0 || f.credentialCalls != 0 || f.dispatches != 0 {
		f.t.Fatalf("call reached provider authority: catalog=%d credentials=%d dispatches=%d", f.catalogCalls, f.credentialCalls, f.dispatches)
	}
}

func succeedDispatch(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
	return map[string]interface{}{"providerResult": "delivered"}, nil
}

func TestExecuteOwnLoopCallDispatchesInlineUnderCallLease(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		call := f.propose("deploy-1", "deploy")
		before := f.currentRun()
		poolClaimedDuringDispatch := false
		var clockTicks atomic.Int64
		worker := f.worker(func(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
			if input.Call.Status != ActionCallStatusRunning || input.Call.LeaseOwner != loopWorkerID || input.Credentials["token"] != "loop-secret" ||
				input.Run == nil || input.Run.ID != call.RunID || input.Arguments["environment"] != "production" {
				t.Fatalf("dispatch input = %#v", input)
			}
			if claimed, err := store.ClaimNextAction(ctx, ActionClaim{Scope: f.scope, WorkerID: "pool-worker", Now: f.now.Add(time.Hour), LeaseDuration: time.Minute}); err != nil || claimed != nil {
				poolClaimedDuringDispatch = true
			}
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(2 * time.Millisecond)
			defer ticker.Stop()
			for {
				current, err := store.GetActionCall(ctx, f.scope, call.ID)
				if err != nil {
					return nil, err
				}
				if current.Revision >= input.Call.Revision+2 {
					return map[string]interface{}{"deploymentId": "deploy-123", "echo": "Bearer loop-secret"}, nil
				}
				select {
				case <-ticker.C:
				case <-deadline.C:
					return nil, errors.New("the loop call lease was not renewed")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		})
		worker.now = func() time.Time { return f.clock.Add(time.Duration(clockTicks.Add(1)) * 10 * time.Millisecond) }
		worker.loopLease = 45 * time.Millisecond
		onClaim := false
		worker.onClaim = func() { onClaim = true }
		result, err := f.execute(worker, call.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Call.Status != ActionCallStatusSucceeded || result.Call.Attempt != 1 || result.Call.LeaseOwner != "" || result.Call.Output["deploymentId"] != "deploy-123" ||
			result.Call.Output["echo"] != "Bearer [REDACTED]" || f.dispatches != 1 || onClaim || poolClaimedDuringDispatch {
			t.Fatalf("inline execution = %#v dispatches=%d onClaim=%t pool=%t", result.Call, f.dispatches, onClaim, poolClaimedDuringDispatch)
		}
		if result.Run != nil {
			t.Fatalf("an unbudgeted loop outcome changed its Run: %#v", result.Run)
		}
		if after := f.currentRun(); after.Revision != before.Revision || after.Status != AgentRunStatusRunning || after.LeaseOwner != loopWorkerID {
			t.Fatalf("loop outcome touched the Run: before=%#v after=%#v", before, after)
		}
		encoded, _ := json.Marshal(result.Event)
		if result.Event.EventType != "action.succeeded" || result.Event.Payload["origin"] != string(ActionOriginLoop) || result.Event.Payload["execution"] != string(ActionExecutionLoop) ||
			strings.Contains(string(encoded), "loop-secret") || strings.Contains(string(encoded), "deploy-123") {
			t.Fatalf("outcome event = %s", encoded)
		}
		assertActionLifecycleTerminalEvents(t, store, call, ActionCallStatusSucceeded, 1)
	})
}

func TestExecuteOwnLoopCallNotClaimable(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		live := f.propose("live-lease", "deploy")
		if claimed, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: f.scope, WorkerID: loopWorkerID, Now: f.clock, LeaseDuration: time.Hour, OwnActionID: live.ID}); err != nil || claimed == nil {
			t.Fatalf("own claim = %#v, %v", claimed, err)
		}
		worker := f.worker(succeedDispatch)
		if _, err := f.execute(worker, live.ID); !errors.Is(err, ErrLoopActionNotClaimable) {
			t.Fatalf("running call with a live lease = %v", err)
		}
		done := f.propose("done", "status")
		if result, err := f.execute(worker, done.ID); err != nil || result.Call.Status != ActionCallStatusSucceeded {
			t.Fatalf("first execution = %#v, %v", result, err)
		}
		if _, err := f.execute(worker, done.ID); !errors.Is(err, ErrLoopActionNotClaimable) || f.dispatches != 1 {
			t.Fatalf("terminal call = %v dispatches=%d", err, f.dispatches)
		}
		if _, err := f.execute(worker, "missing"); !errors.Is(err, ErrActionNotFound) {
			t.Fatalf("missing call = %v", err)
		}
	})
}

func TestExecuteOwnLoopCallNotOwnedAfterLeaseLossCancels(t *testing.T) {
	for _, loss := range []string{"foreign_worker", "expired_run_lease", "parked_run", "canceled_during_credentials"} {
		t.Run(loss, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				f := newLoopWorkerFixture(t, store, &BudgetPolicy{MaxActions: 10})
				call := f.propose("deploy-1", "deploy")
				worker := f.worker(succeedDispatch)
				workerID := loopWorkerID
				switch loss {
				case "foreign_worker":
					workerID = "other-replica"
				case "expired_run_lease":
					f.clock = f.now.Add(2 * time.Hour)
				case "parked_run":
					f.setRunStatus(AgentRunStatusWaitingForApproval)
				case "canceled_during_credentials":
					worker.credentials = CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
						f.credentialCalls++
						f.setRunStatus(AgentRunStatusCanceled)
						return map[string]string{"token": "loop-secret"}, nil
					})
				}
				before := f.currentRun()
				result, err := worker.ExecuteOwnLoopCall(t.Context(), f.scope, call.ID, workerID)
				if err != nil {
					t.Fatal(err)
				}
				if result.Call.Status != ActionCallStatusCanceled || result.Call.CompletedAt == nil || result.Call.LeaseOwner != "" ||
					result.Call.Error != "owning Run no longer waits for this action" || f.dispatches != 0 {
					t.Fatalf("unowned loop call = %#v dispatches=%d", result.Call, f.dispatches)
				}
				if loss != "canceled_during_credentials" && (f.catalogCalls != 0 || f.credentialCalls != 0) {
					t.Fatalf("unowned call resolved authority: catalog=%d credentials=%d", f.catalogCalls, f.credentialCalls)
				}
				if loss == "canceled_during_credentials" {
					before = f.run
				}
				after := f.currentRun()
				if after.Status != before.Status || after.LeaseOwner != before.LeaseOwner || !reflect.DeepEqual(after.WakeCondition, before.WakeCondition) {
					t.Fatalf("cancellation changed the Run: before=%#v after=%#v", before, after)
				}
				if _, reserved := after.BudgetReservations[actionBudgetReservationID(call.ID)]; reserved || after.BudgetUsage.Actions != 0 {
					t.Fatalf("cancellation did not release the action reservation: %#v", after)
				}
				assertActionLifecycleTerminalEvents(t, store, call, ActionCallStatusCanceled, 0)
			})
		})
	}
}

func TestLoopRecoveredRunningWriteFailsWithoutDispatch(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, &BudgetPolicy{MaxActions: 10})
		call := f.propose("deploy-1", "deploy")
		first, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: f.scope, WorkerID: loopWorkerID, Now: f.clock, LeaseDuration: LoopActionLease, OwnActionID: call.ID})
		if err != nil || first == nil {
			t.Fatalf("crashed claim = %#v, %v", first, err)
		}
		f.clock = first.LeaseExpiresAt.Add(time.Millisecond)
		result, err := f.execute(f.worker(succeedDispatch), call.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Call.Status != ActionCallStatusFailed || !result.Call.RecoveredRunning || result.Call.FailurePhase != ActionFailureAfterDispatch || result.Call.Attempt != 2 ||
			!strings.Contains(result.Call.Error, "may already have taken effect") {
			t.Fatalf("recovered write = %#v", result.Call)
		}
		f.requireNoAuthority()
		after := f.currentRun()
		if after.Status != AgentRunStatusRunning || after.LeaseOwner != loopWorkerID || after.BudgetUsage.Actions != 1 || len(after.BudgetReservations) != 0 {
			t.Fatalf("recovered write changed the Run beyond its budget: %#v", after)
		}
		assertActionLifecycleUnclaimable(t, store, call, f.clock.Add(time.Hour))
	})
}

func TestRecoveredRunningWriteHasInterruptedCode(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		write := f.propose("deploy-1", "deploy")
		read := f.propose("status-1", "status")
		for _, call := range []*ActionCall{write, read} {
			if claimed, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: f.scope, WorkerID: loopWorkerID, Now: f.clock, LeaseDuration: LoopActionLease, OwnActionID: call.ID}); err != nil || claimed == nil {
				t.Fatalf("crashed claim = %#v, %v", claimed, err)
			}
		}
		f.clock = f.clock.Add(LoopActionLease + time.Second)
		worker := f.worker(succeedDispatch)
		interrupted, err := f.execute(worker, write.ID)
		if err != nil {
			t.Fatal(err)
		}
		if interrupted.Call.ErrorCode != ActionErrorCodeInterrupted || interrupted.Call.ErrorDetails != nil {
			t.Fatalf("recovered write error code = %#v", interrupted.Call)
		}
		stored := f.call(write.ID)
		if stored.Status != ActionCallStatusFailed || stored.ErrorCode != ActionErrorCodeInterrupted {
			t.Fatalf("stored recovered write = %#v", stored)
		}
		reread, err := f.execute(worker, read.ID)
		if err != nil || reread.Call.Status != ActionCallStatusSucceeded || reread.Call.ErrorCode != "" {
			t.Fatalf("recovered read = %#v, %v", reread, err)
		}
	})
}

func TestLoopRecoveredRunningReadRedispatches(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		call := f.propose("status-1", "status")
		first, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: f.scope, WorkerID: loopWorkerID, Now: f.clock, LeaseDuration: LoopActionLease, OwnActionID: call.ID})
		if err != nil || first == nil {
			t.Fatalf("crashed claim = %#v, %v", first, err)
		}
		f.clock = first.LeaseExpiresAt.Add(time.Millisecond)
		result, err := f.execute(f.worker(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"healthy": true}, nil
		}), call.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Call.Status != ActionCallStatusSucceeded || !result.Call.RecoveredRunning || result.Call.FailurePhase != "" || result.Call.Attempt != 2 || result.Call.Output["healthy"] != true {
			t.Fatalf("recovered read = %#v", result.Call)
		}
		if f.catalogCalls != 1 || f.credentialCalls != 1 || f.dispatches != 1 {
			t.Fatalf("recovered read authority: catalog=%d credentials=%d dispatches=%d", f.catalogCalls, f.credentialCalls, f.dispatches)
		}
	})
}

func TestLoopMaintenanceAtExecutionParks(t *testing.T) {
	for _, boundary := range []string{"before_claim", "after_claim", "after_catalog"} {
		t.Run(boundary, func(t *testing.T) {
			forActionLifecycleBackends(t, func(t *testing.T, store KernelStore, backend actionLifecycleBackend) {
				if boundary == "before_claim" {
					if _, ok := store.(*SQLiteStore); !ok {
						t.Skip("the store-side claim gate is seeded through the SQLite store")
					}
				}
				f := newLoopWorkerFixture(t, store, &BudgetPolicy{MaxActions: 10})
				call := f.propose("deploy-1", "deploy")
				before := f.currentRun()
				gate := &SkillRuntimeMaintenance{Scope: f.scope, SkillID: "release", OperationID: "upgrade-release", Owner: "controller", Active: true, Revision: 1}
				wrapped := &loopMaintenanceStore{KernelStore: store, gate: gate, active: boundary != "after_catalog"}
				if boundary == "before_claim" {
					if err := writeSkillRuntimeMaintenance(t.Context(), backend.db, "skill_runtime_maintenance", gate, false); err != nil {
						t.Fatal(err)
					}
				}
				catalogCalls := 0
				catalog := &actionRuntimeMaintenanceCatalog{ActionExecutionCatalog: f.catalog, calls: &catalogCalls}
				if boundary == "after_catalog" {
					catalog.afterResolve = func() { wrapped.active = true }
				}
				worker := NewActionWorker(wrapped, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
					f.credentialCalls++
					return map[string]string{"token": "loop-secret"}, nil
				}), ActionDispatcherFunc(func(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
					f.dispatches++
					return succeedDispatch(ctx, input)
				}))
				worker.now = func() time.Time { return f.clock }
				result, err := f.execute(worker, call.ID)
				var held *LoopActionMaintenanceError
				if result != nil || !errors.As(err, &held) || !errors.Is(err, ErrSkillRuntimeMaintenance) {
					t.Fatalf("maintenance at execution = %#v, %v", result, err)
				}
				if held.Maintenance.OperationID != "upgrade-release" || !held.RetryAt.Equal(f.clock.Add(loopActionMaintenanceRetry)) ||
					held.Call == nil || held.Call.ID != call.ID || held.Call.Status != ActionCallStatusReady {
					t.Fatalf("maintenance park = %#v", held)
				}
				if f.dispatches != 0 || boundary != "after_catalog" && (catalogCalls != 0 || f.credentialCalls != 0) {
					t.Fatalf("maintenance reached authority: catalog=%d credentials=%d dispatches=%d", catalogCalls, f.credentialCalls, f.dispatches)
				}
				stored := f.call(call.ID)
				if stored.Status != ActionCallStatusReady || stored.CompletedAt != nil || stored.LeaseOwner != "" || stored.Error != "" {
					t.Fatalf("maintenance closed the call: %#v", stored)
				}
				if after := f.currentRun(); !reflect.DeepEqual(before, after) {
					t.Fatalf("maintenance changed the Run: before=%#v after=%#v", before, after)
				}

				// Maintenance ends: the same call executes on resume.
				wrapped.active = false
				if boundary == "before_claim" {
					execSQL(t, backend.db, `DELETE FROM skill_runtime_maintenance WHERE scope_kind = ? AND scope_id = ? AND skill_id = ?`, f.scope.Kind, f.scope.ID, "release")
				}
				catalog.afterResolve = nil
				f.clock = held.RetryAt
				resumed, err := f.execute(worker, call.ID)
				if err != nil || resumed.Call.Status != ActionCallStatusSucceeded || f.dispatches != 1 {
					t.Fatalf("resumed call = %#v, %v dispatches=%d", resumed, err, f.dispatches)
				}
				assertActionLifecycleTerminalEvents(t, store, call, ActionCallStatusSucceeded, 1)
			})
		})
	}
}

type loopMaintenanceStore struct {
	KernelStore
	gate   *SkillRuntimeMaintenance
	active bool
}

func (s *loopMaintenanceStore) GetSkillRuntimeMaintenance(ctx context.Context, scope Scope, skillID string) (*SkillRuntimeMaintenance, error) {
	if s.active && scope == s.gate.Scope && skillID == s.gate.SkillID {
		gate := *s.gate
		return &gate, nil
	}
	return nil, ErrSkillRuntimeMaintenanceNotFound
}

func TestLoopOutcomeLeavesRunUnchangedExceptBudgetAndHumanIntervention(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		succeeded := f.propose("status-ok", "status")
		failed := f.propose("deploy-fails", "deploy")
		human := f.propose("status-human", "status")
		before := f.currentRun()
		worker := f.worker(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
			switch input.Call.ID {
			case failed.ID:
				return nil, errors.New("provider rejected loop-secret")
			case human.ID:
				return map[string]interface{}{"requiresHuman": true, "challenges": []interface{}{"captcha", "captcha"}, "currentUrl": "https://example.invalid/?t=loop-secret"}, nil
			}
			return map[string]interface{}{"healthy": true}, nil
		})
		ids := []string{"event-ok", "human-request", "event-human", "event-failed"}
		worker.newID = func() string {
			id := ids[0]
			ids = ids[1:]
			return id
		}
		ok, err := f.execute(worker, succeeded.ID)
		if err != nil || ok.Call.Status != ActionCallStatusSucceeded || ok.Run != nil {
			t.Fatalf("succeeded = %#v, %v", ok, err)
		}
		requested, err := f.execute(worker, human.ID)
		if err != nil || requested.Call.Status != ActionCallStatusSucceeded || requested.Run == nil {
			t.Fatalf("human intervention = %#v, %v", requested, err)
		}
		if requested.Run.Status != AgentRunStatusRunning || requested.Run.WakeCondition != nil || requested.Run.LeaseOwner != loopWorkerID || len(requested.Run.HumanInterventions) != 1 {
			t.Fatalf("human intervention changed more than the request list: %#v", requested.Run)
		}
		request := requested.Run.HumanInterventions[0]
		encoded, _ := json.Marshal(request)
		if request.ID != "human-request" || request.ActionCallID != human.ID || request.Status != HumanInterventionStatusPending ||
			len(request.Challenge) != 1 || request.Challenge[0] != "captcha" || strings.Contains(string(encoded), "loop-secret") {
			t.Fatalf("human intervention request = %s", encoded)
		}
		if requested.Event.EventType != "action.human_intervention_required" || requested.Event.Payload["humanInterventionId"] != "human-request" || requested.Event.Severity != ActivitySeverityWarning {
			t.Fatalf("human intervention event = %#v", requested.Event)
		}
		failure, err := f.execute(worker, failed.ID)
		if err != nil || failure.Call.Status != ActionCallStatusFailed || failure.Run != nil || failure.Call.FailurePhase != ActionFailureAfterDispatch ||
			!strings.Contains(failure.Call.Error, "[REDACTED]") || strings.Contains(failure.Call.Error, "loop-secret") || failure.Call.ErrorCode != "" {
			t.Fatalf("failed = %#v, %v", failure, err)
		}
		after := f.currentRun()
		if after.Status != before.Status || after.LeaseOwner != before.LeaseOwner || !reflect.DeepEqual(after.Checkpoint, before.Checkpoint) ||
			after.WakeCondition != nil || after.Error != "" || after.Revision != before.Revision+1 || len(after.HumanInterventions) != 1 {
			t.Fatalf("loop outcomes changed the Run: before=%#v after=%#v", before, after)
		}
	})
	t.Run("budget", func(t *testing.T) {
		forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
			f := newLoopWorkerFixture(t, store, &BudgetPolicy{MaxActions: 10})
			call := f.propose("deploy-1", "deploy")
			reserved := f.currentRun()
			if _, ok := reserved.BudgetReservations[actionBudgetReservationID(call.ID)]; !ok {
				t.Fatalf("fixture did not reserve the action: %#v", reserved)
			}
			result, err := f.execute(f.worker(succeedDispatch), call.ID)
			if err != nil || result.Run == nil {
				t.Fatalf("budgeted outcome = %#v, %v", result, err)
			}
			after := f.currentRun()
			if after.BudgetUsage.Actions != 1 || len(after.BudgetReservations) != 0 || after.Status != AgentRunStatusRunning ||
				after.LeaseOwner != loopWorkerID || !reflect.DeepEqual(after.Checkpoint, reserved.Checkpoint) || after.Revision != reserved.Revision+1 {
				t.Fatalf("budget settlement changed more than the budget: before=%#v after=%#v", reserved, after)
			}
		})
	})
}

func TestLoopConnectAccountParkedCallKeepsCardOutput(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	proposal := proposeLoopAction(t, store, catalog, run, "agent-worker", now, loopActionSpec{
		id: "send-1", skillID: "mail", version: "1.0.0", action: "send", arguments: map[string]interface{}{"to": "ana@example.com"},
		externalOperation: &ExternalOperationIdentity{Resource: "mailbox-ana", Operation: "send"},
	})
	if proposal.Call.ExternalOperationDigest == "" {
		t.Fatal("test needs an external operation")
	}
	before, _ := store.GetAgentRun(t.Context(), scope, run.ID)
	dispatches := 0
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"google": "token"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return map[string]interface{}{"sent": true}, nil
	}))
	worker.now = func() time.Time { return now.Add(time.Second) }
	worker.setCredentialAvailabilityChecker(availabilityFunc(func(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error) {
		return CredentialAvailability{Gaps: map[string]CredentialGap{"google": CredentialGapPersonSignIn}}, nil
	}))
	result, err := worker.ExecuteOwnLoopCall(t.Context(), scope, proposal.Call.ID, "agent-worker")
	if err != nil {
		t.Fatal(err)
	}
	call := result.Call
	if dispatches != 0 || call.Status != ActionCallStatusFailed || call.FailurePhase != ActionFailureBeforeDispatch ||
		!strings.HasPrefix(call.Error, "connect_account_required: ") || call.ExternalOperationDigest != "" || !isParkedConnectAccountCall(call) {
		t.Fatalf("parked call = %#v", call)
	}
	card, err := store.GetSkillSetupRequest(t.Context(), scope, "connect-account:"+call.ID)
	if err != nil || card == nil || card.Status != "pending" || card.RunID != run.ID {
		t.Fatalf("card = %#v, %v", card, err)
	}
	if call.Output["message"] != card.Reason {
		t.Fatalf("parked call lost its card output: %#v", call.Output)
	}
	if result.Event.EventType != "action.connection_requested" || result.Run != nil {
		t.Fatalf("parked outcome = event %#v run %#v", result.Event, result.Run)
	}
	after, _ := store.GetAgentRun(t.Context(), scope, run.ID)
	if after.Status != AgentRunStatusRunning || after.WakeCondition != nil || after.Revision != before.Revision {
		t.Fatalf("a parked loop call parked the Run itself: %#v", after)
	}
}

func TestPoolClosesAbandonedLoopCallOfEndedRun(t *testing.T) {
	for _, ended := range []AgentRunStatus{AgentRunStatusCanceled, AgentRunStatusFailed, AgentRunStatusCompleted} {
		t.Run(string(ended), func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				f := newLoopWorkerFixture(t, store, &BudgetPolicy{MaxActions: 10})
				ready := f.propose("ready-call", "deploy")
				running := f.propose("running-call", "status")
				if claimed, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: f.scope, WorkerID: loopWorkerID, Now: f.clock, LeaseDuration: LoopActionLease, OwnActionID: running.ID}); err != nil || claimed == nil {
					t.Fatalf("loop claim = %#v, %v", claimed, err)
				}
				f.setRunStatus(ended)
				endedRun := f.currentRun()
				f.clock = f.clock.Add(LoopActionLease + time.Second)
				worker := f.worker(succeedDispatch)
				closed := map[string]*ActionCall{}
				for range 2 {
					result, err := worker.RunOnce(t.Context(), f.scope, "pool-worker", time.Minute)
					if err != nil || result == nil {
						t.Fatalf("pool close = %#v, %v", result, err)
					}
					closed[result.Call.ID] = result.Call
				}
				f.requireNoAuthority()
				if call := closed[ready.ID]; call == nil || call.Status != ActionCallStatusCanceled || call.Error != errLoopActionAbandoned.Error() || call.CompletedAt == nil {
					t.Fatalf("abandoned ready call = %#v", call)
				}
				if call := closed[running.ID]; call == nil || call.Status != ActionCallStatusFailed || call.FailurePhase != ActionFailureAfterDispatch ||
					call.Error != errLoopActionAbandoned.Error() || !call.RecoveredRunning {
					t.Fatalf("abandoned running call = %#v", call)
				}
				after := f.currentRun()
				if after.Status != endedRun.Status || len(after.BudgetReservations) != 0 || after.BudgetUsage.Actions != 1 || !reflect.DeepEqual(after.Checkpoint, endedRun.Checkpoint) {
					t.Fatalf("closing abandoned calls changed the ended Run beyond its budget: %#v", after)
				}
				assertActionLifecycleUnclaimable(t, store, ready, f.clock.Add(time.Hour))
			})
		})
	}
}

func TestPoolWorkerNeverRunsLoopCallOfLiveRun(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusRunning, AgentRunStatusQueued, AgentRunStatusWaitingForApproval, AgentRunStatusWaitingForEvent, AgentRunStatusPaused} {
		t.Run(string(status), func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				f := newLoopWorkerFixture(t, store, nil)
				call := f.propose("deploy-1", "deploy")
				if status != AgentRunStatusRunning {
					f.setRunStatus(status)
				}
				worker := f.worker(succeedDispatch)
				f.clock = f.clock.Add(time.Hour)
				if result, err := worker.RunOnce(t.Context(), f.scope, "pool-worker", time.Minute); err != nil || result != nil {
					t.Fatalf("pool took a live Run's loop call: %#v, %v", result, err)
				}
				f.requireNoAuthority()
				if stored := f.call(call.ID); stored.Status != ActionCallStatusReady || stored.Attempt != 0 {
					t.Fatalf("pool touched the loop call: %#v", stored)
				}
			})
		})
	}
}
