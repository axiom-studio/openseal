package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// runLoopStoreScenario runs scenario on memory and SQLite, and on Postgres
// when OPENSEAL_TEST_POSTGRES_DSN is set (Postgres-only checks live in
// action_loop_postgres_integration_test.go).
func runLoopStoreScenario(t *testing.T, scenario func(*testing.T, KernelStore)) {
	t.Helper()
	for _, backend := range nestedActionStores(t) {
		t.Run(backend.name, func(t *testing.T) { scenario(t, backend.open(t)) })
	}
}

const loopStoreWorker = "loop-worker"

// loopStoreFixture is one running Run claimed by loopStoreWorker under a live
// lease, whose step "step-1" proposes loop calls.
type loopStoreFixture struct {
	t     *testing.T
	ctx   context.Context
	store KernelStore
	scope Scope
	now   time.Time
	run   *AgentRun
}

func newLoopStoreFixture(t *testing.T, store KernelStore) *loopStoreFixture {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "loop-" + uuid.NewString()[:8]}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	created, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "check the repo", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: loopStoreWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	return &loopStoreFixture{t: t, ctx: ctx, store: store, scope: scope, now: now, run: run}
}

// loopCall is a ready loop call of step-1 with its own idempotency key.
func (f *loopStoreFixture) loopCall(id string) *ActionCall {
	call := &ActionCall{
		ID: id, Scope: f.scope, RunID: f.run.ID, TurnID: "step-1", DeploymentID: "agent",
		SkillID: "repo", SkillVersion: "1.0.0", Action: "read_file", Origin: ActionOriginLoop, Execution: ActionExecutionLoop,
		Status: ActionCallStatusReady, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
		Arguments: map[string]interface{}{"path": id}, IdempotencyKey: "tool-call:" + id,
		MaxAttempts: 1, AvailableAt: f.now, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	return call
}

func (f *loopStoreFixture) event(id, eventType string) *ActivityEvent {
	return &ActivityEvent{
		ID: id, Scope: f.scope, EventType: eventType, Severity: ActivitySeverityInfo, AgentID: "agent", RunID: f.run.ID,
		Actor: ActivityActor{Type: "worker", ID: loopStoreWorker}, Summary: "Loop call", Visibility: ActivityVisibilityScope, CreatedAt: f.now,
	}
}

// proposal is call's loop proposal without a Run change, fenced by the live
// Run lease.
func (f *loopStoreFixture) proposal(call *ActionCall) ActionProposalRecord {
	return ActionProposalRecord{
		Call: call, ExpectedRunRevision: f.run.Revision, Lease: &AgentRunLeaseGuard{WorkerID: loopStoreWorker, Now: f.now},
		Event: f.event("event-"+call.ID, "action.proposed"),
	}
}

func (f *loopStoreFixture) propose(call *ActionCall) *ActionCall {
	f.t.Helper()
	result, err := f.store.CreateActionProposal(f.ctx, f.proposal(call))
	if err != nil || result == nil || !result.Created {
		f.t.Fatalf("propose %s = %#v, %v", call.ID, result, err)
	}
	return result.Call
}

func (f *loopStoreFixture) currentRun() *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.scope, f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// setRunStatus moves the Run to status outside any lease, as a park,
// requeue, pause or cancel would.
func (f *loopStoreFixture) setRunStatus(status AgentRunStatus) {
	f.t.Helper()
	current := f.currentRun()
	updated := cloneAgentRun(current)
	updated.Status = status
	updated.Revision++
	updated.UpdatedAt = f.now
	if status != AgentRunStatusRunning {
		updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	}
	if isTerminalAgentRunStatus(status) {
		updated.CompletedAt = &f.now
	}
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, f.event("status-"+string(status)+"-"+uuid.NewString()[:8], "run.status_changed"), nil); err != nil {
		f.t.Fatalf("set Run %s: %v", status, err)
	}
	f.run = f.currentRun()
}

func (f *loopStoreFixture) poolClaim(at time.Time) *ActionCall {
	f.t.Helper()
	claimed, err := f.store.ClaimNextAction(f.ctx, ActionClaim{Scope: f.scope, WorkerID: "pool-worker", Now: at, LeaseDuration: time.Minute})
	if err != nil {
		f.t.Fatal(err)
	}
	return claimed
}

func (f *loopStoreFixture) ownClaim(id string, at time.Time) *ActionCall {
	f.t.Helper()
	claimed, err := f.store.ClaimNextAction(f.ctx, ActionClaim{Scope: f.scope, WorkerID: loopStoreWorker, Now: at, LeaseDuration: LoopActionLease, OwnActionID: id})
	if err != nil {
		f.t.Fatal(err)
	}
	return claimed
}

func (f *loopStoreFixture) requireNoCall(id string) {
	f.t.Helper()
	if call, err := f.store.GetActionCall(f.ctx, f.scope, id); !errors.Is(err, ErrActionNotFound) {
		f.t.Fatalf("call %s = %#v, %v; want none", id, call, err)
	}
}

func TestLoopProposalRequiresRunningRunAndLiveLease(t *testing.T) {
	runLoopStoreScenario(t, loopProposalRequiresRunningRunAndLiveLease)
}

func loopProposalRequiresRunningRunAndLiveLease(t *testing.T, store KernelStore) {
	f := newLoopStoreFixture(t, store)
	expired := f.proposal(f.loopCall("expired-lease"))
	expired.Lease.Now = f.now.Add(2 * time.Hour)
	if _, err := store.CreateActionProposal(f.ctx, expired); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease = %v", err)
	}
	f.requireNoCall("expired-lease")
	foreign := f.proposal(f.loopCall("foreign-owner"))
	foreign.Lease.WorkerID = "other-worker"
	if _, err := store.CreateActionProposal(f.ctx, foreign); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign owner = %v", err)
	}
	f.requireNoCall("foreign-owner")
	unleased := f.proposal(f.loopCall("no-lease"))
	unleased.Lease = nil
	if _, err := store.CreateActionProposal(f.ctx, unleased); err == nil {
		t.Fatal("a loop proposal without its Run lease was accepted")
	}
	f.requireNoCall("no-lease")

	// A queued Run whose lease fields survived is still not running.
	current := f.currentRun()
	queued := cloneAgentRun(current)
	queued.Status = AgentRunStatusQueued
	queued.Revision++
	if _, err := store.UpdateAgentRunWithEvent(f.ctx, queued, current.Revision, f.event("queued", "run.status_changed"), nil); err != nil {
		t.Fatal(err)
	}
	f.run = f.currentRun()
	if _, err := store.CreateActionProposal(f.ctx, f.proposal(f.loopCall("queued-run"))); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("queued Run = %v", err)
	}
	f.requireNoCall("queued-run")
}

func TestLoopProposalWithoutRunUpdateIgnoresRunRevision(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		observed := f.run.Revision
		heartbeat, err := store.RenewAgentRunLease(f.ctx, f.scope, f.run.ID, loopStoreWorker, f.now, time.Hour)
		if err != nil || heartbeat.Revision == observed {
			t.Fatalf("heartbeat = %#v, %v", heartbeat, err)
		}
		proposal := f.proposal(f.loopCall("after-heartbeat"))
		proposal.ExpectedRunRevision = observed
		result, err := store.CreateActionProposal(f.ctx, proposal)
		if err != nil || !result.Created || result.Call.ID != "after-heartbeat" {
			t.Fatalf("proposal = %#v, %v", result, err)
		}
		if run := f.currentRun(); run.Revision != heartbeat.Revision || run.Status != AgentRunStatusRunning {
			t.Fatalf("Run changed: %#v", run)
		}
	})
}

func TestLoopProposalBudgetReservationIsRevisionFenced(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		reserve := func(observed *AgentRun) *AgentRun {
			updated := cloneAgentRun(observed)
			updated.Revision++
			updated.UpdatedAt = f.now
			return updated
		}
		observed := f.currentRun()
		if _, err := store.RenewAgentRunLease(f.ctx, f.scope, f.run.ID, loopStoreWorker, f.now, time.Hour); err != nil {
			t.Fatal(err)
		}
		stale := f.proposal(f.loopCall("stale-reservation"))
		stale.Run, stale.ExpectedRunRevision = reserve(observed), observed.Revision
		if _, err := store.CreateActionProposal(f.ctx, stale); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("stale reservation = %v", err)
		}
		f.requireNoCall("stale-reservation")
		current := f.currentRun()
		fresh := f.proposal(f.loopCall("fresh-reservation"))
		fresh.Run, fresh.ExpectedRunRevision = reserve(current), current.Revision
		if result, err := store.CreateActionProposal(f.ctx, fresh); err != nil || !result.Created {
			t.Fatalf("fresh reservation = %#v, %v", result, err)
		}
		if run := f.currentRun(); run.Revision != current.Revision+1 {
			t.Fatalf("reserved Run revision = %d, want %d", run.Revision, current.Revision+1)
		}
	})
}

func TestOriginlessProposalIsRejected(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		call := f.loopCall("originless")
		call.Origin, call.Execution = "", ""
		proposal := f.proposal(call)
		updated := cloneAgentRun(f.run)
		updated.Revision++
		proposal.Run = updated
		if _, err := store.CreateActionProposal(f.ctx, proposal); err == nil || !strings.Contains(err.Error(), "action origin is invalid") {
			t.Fatalf("originless proposal = %v", err)
		}
		f.requireNoCall("originless")
	})
}

func TestLoopActionCallValidation(t *testing.T) {
	f := &loopStoreFixture{scope: Scope{Kind: "tenant", ID: "one"}, now: time.Now().UTC(), run: &AgentRun{ID: "run"}}
	if err := f.loopCall("valid").Validate(); err != nil {
		t.Fatalf("loop call = %v", err)
	}
	for name, mutate := range map[string]func(*ActionCall){
		"no origin":       func(c *ActionCall) { c.Origin, c.Execution = "", "" },
		"unknown origin":  func(c *ActionCall) { c.Origin = "model" },
		"no step":         func(c *ActionCall) { c.TurnID = "" },
		"worker executed": func(c *ActionCall) { c.Execution = "" },
		"host executed":   func(c *ActionCall) { c.Execution = ActionExecutionHost },
		"origin ref":      func(c *ActionCall) { c.OriginRef = "command-1" },
		"vm loop execute": func(c *ActionCall) { c.Origin = ActionOriginVM },
	} {
		call := f.loopCall("invalid")
		mutate(call)
		if err := call.Validate(); err == nil {
			t.Errorf("%s: invalid loop call validated", name)
		}
	}
	vm := f.loopCall("vm")
	vm.Origin, vm.Execution, vm.OriginRef = ActionOriginVM, ActionExecutionHost, "command-1"
	if err := vm.Validate(); err != nil {
		t.Fatalf("vm host call = %v", err)
	}
	vm.Execution = ""
	if err := vm.Validate(); err != nil {
		t.Fatalf("vm worker call = %v", err)
	}
}

func TestRefusedStatusValidatesWithDuplicateOf(t *testing.T) {
	f := &loopStoreFixture{scope: Scope{Kind: "tenant", ID: "one"}, now: time.Now().UTC(), run: &AgentRun{ID: "run"}}
	refused := f.loopCall("refused")
	refused.Status = ActionCallStatusRefused
	refused.DuplicateOfActionCallID = "first-call"
	refused.ErrorCode = "repeat_refused"
	refused.ErrorDetails = map[string]string{"step": "2"}
	refused.CompletedAt = &f.now
	if err := refused.Validate(); err != nil {
		t.Fatalf("refused duplicate = %v", err)
	}
	failed := f.loopCall("failed")
	failed.Status = ActionCallStatusFailed
	failed.DuplicateOfActionCallID = "first-call"
	if err := failed.Validate(); err == nil {
		t.Fatal("a non-refused duplicate validated without an external operation digest")
	}
	if !validActionCallStatus(ActionCallStatusRefused) || externalOperationProtects(ActionCallStatusRefused) {
		t.Fatal("refused must be a valid status that never protects an external operation")
	}
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		row := f.loopCall("refused-row")
		row.Status, row.DuplicateOfActionCallID, row.ErrorCode, row.CompletedAt = ActionCallStatusRefused, "first-call", "duplicate_suppressed", &f.now
		f.propose(row)
		if claimed := f.poolClaim(f.now.Add(time.Hour)); claimed != nil {
			t.Fatalf("refused call claimed: %#v", claimed)
		}
		f.setRunStatus(AgentRunStatusCanceled)
		if claimed := f.poolClaim(f.now.Add(time.Hour)); claimed != nil {
			t.Fatalf("refused call of ended Run claimed: %#v", claimed)
		}
		stored, err := store.GetActionCall(f.ctx, f.scope, "refused-row")
		if err != nil || stored.Status != ActionCallStatusRefused || stored.DuplicateOfActionCallID != "first-call" || stored.ErrorCode != "duplicate_suppressed" {
			t.Fatalf("stored refused row = %#v, %v", stored, err)
		}
	})
}

func TestPoolNeverClaimsLoopCallOfLiveRun(t *testing.T) {
	runLoopStoreScenario(t, poolNeverClaimsLoopCallOfLiveRun)
}

func poolNeverClaimsLoopCallOfLiveRun(t *testing.T, store KernelStore) {
	for _, status := range []AgentRunStatus{
		AgentRunStatusQueued, AgentRunStatusRunning, AgentRunStatusWaitingForApproval, AgentRunStatusWaitingForEvent, AgentRunStatusPaused,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newLoopStoreFixture(t, store)
			f.propose(f.loopCall("ready-call"))
			f.propose(f.loopCall("abandoned-call"))
			if claimed := f.ownClaim("abandoned-call", f.now); claimed == nil || claimed.Status != ActionCallStatusRunning {
				t.Fatalf("own claim = %#v", claimed)
			}
			if status != AgentRunStatusRunning {
				f.setRunStatus(status)
			}
			// Well past the loop's call lease: the running call is expired too.
			if claimed := f.poolClaim(f.now.Add(10 * LoopActionLease)); claimed != nil {
				t.Fatalf("pool claimed loop call %s of a %s Run", claimed.ID, status)
			}
		})
	}
}

func TestPoolClaimsLoopCallOfEndedRun(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		f.propose(f.loopCall("ready-call"))
		f.setRunStatus(AgentRunStatusCanceled)
		claimed := f.poolClaim(f.now.Add(time.Second))
		if claimed == nil || claimed.ID != "ready-call" || claimed.Status != ActionCallStatusRunning || claimed.LeaseOwner != "pool-worker" {
			t.Fatalf("pool claim of ended Run's call = %#v", claimed)
		}
	})
}

func TestOwnClaimTakesReadyOrExpiredRunningLoopCall(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		f.propose(f.loopCall("own-call"))
		f.propose(f.loopCall("other-call"))
		first := f.ownClaim("own-call", f.now)
		if first == nil || first.ID != "own-call" || first.Status != ActionCallStatusRunning || first.LeaseOwner != loopStoreWorker || first.Attempt != 1 || first.RecoveredRunning {
			t.Fatalf("own claim of ready call = %#v", first)
		}
		if live := f.ownClaim("own-call", f.now.Add(LoopActionLease/2)); live != nil {
			t.Fatalf("own claim took a running call under a live lease: %#v", live)
		}
		recovered := f.ownClaim("own-call", f.now.Add(2*LoopActionLease))
		if recovered == nil || recovered.ID != "own-call" || recovered.Attempt != 2 || !recovered.RecoveredRunning {
			t.Fatalf("own claim of expired running call = %#v", recovered)
		}
		if missing := f.ownClaim("unknown-call", f.now); missing != nil {
			t.Fatalf("own claim of an unknown id = %#v", missing)
		}
	})
}

func TestGetActionCallByIdempotencyKey(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		f.propose(f.loopCall("keyed-call"))
		found, err := store.GetActionCallByIdempotencyKey(f.ctx, f.scope, f.run.ID, "tool-call:keyed-call")
		if err != nil || found == nil || found.ID != "keyed-call" || found.Origin != ActionOriginLoop || found.Execution != ActionExecutionLoop {
			t.Fatalf("lookup = %#v, %v", found, err)
		}
		for name, lookup := range map[string]func() (*ActionCall, error){
			"other key": func() (*ActionCall, error) {
				return store.GetActionCallByIdempotencyKey(f.ctx, f.scope, f.run.ID, "tool-call:missing")
			},
			"other run": func() (*ActionCall, error) {
				return store.GetActionCallByIdempotencyKey(f.ctx, f.scope, "other-run", "tool-call:keyed-call")
			},
			"other scope": func() (*ActionCall, error) {
				return store.GetActionCallByIdempotencyKey(f.ctx, Scope{Kind: "tenant", ID: "other"}, f.run.ID, "tool-call:keyed-call")
			},
			"empty key": func() (*ActionCall, error) { return store.GetActionCallByIdempotencyKey(f.ctx, f.scope, f.run.ID, "") },
		} {
			if call, err := lookup(); !errors.Is(err, ErrActionNotFound) {
				t.Errorf("%s = %#v, %v; want ErrActionNotFound", name, call, err)
			}
		}
	})
}

func TestLoopApprovalResolutionWithoutRunIsValid(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		call := f.loopCall("reviewed-call")
		call.Action, call.Risk, call.SideEffect = "push", skill.RiskLevelExternal, skill.SideEffectExternal
		call.Status, call.ApprovalID = ActionCallStatusWaitingApproval, "approval-1"
		call.InvocationDigest = ComputeActionInvocationDigest(call)
		call.SemanticDigest = ComputeActionSemanticDigest(call)
		approval := &ApprovalCheckpoint{
			ID: "approval-1", Scope: f.scope, RunID: f.run.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
			Risk: skill.RiskLevelExternal, Summary: "Push the branch", ProposedAction: map[string]interface{}{"path": call.ID},
			EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "user"}}, ExpiresAt: f.now.Add(time.Hour), Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
		}
		proposal := f.proposal(call)
		proposal.Approval = approval
		if result, err := store.CreateActionProposal(f.ctx, proposal); err != nil || !result.Created {
			t.Fatalf("proposal = %#v, %v", result, err)
		}
		before := f.currentRun()
		decided := cloneApprovalCheckpoint(approval)
		decided.Status, decided.DecisionID, decided.DecisionBy, decided.DecidedAt = ApprovalStatusApproved, "decision-1", &ApprovalPrincipal{Type: "user", ID: "user"}, &f.now
		decided.Revision++
		decided.UpdatedAt = f.now
		ready := cloneActionCall(call)
		ready.Status = ActionCallStatusReady
		ready.Revision++
		ready.UpdatedAt = f.now
		result, err := store.ResolveApproval(f.ctx, ApprovalResolutionRecord{
			Approval: decided, ExpectedApprovalRevision: approval.Revision, Call: ready, ExpectedCallRevision: call.Revision,
			Event: f.event("approval-decided", "approval.approved"),
		})
		if err != nil || !result.Resolved || result.Call.Status != ActionCallStatusReady {
			t.Fatalf("resolution = %#v, %v", result, err)
		}
		if after := f.currentRun(); after.Revision != before.Revision || after.Status != AgentRunStatusRunning {
			t.Fatalf("resolution without a Run update changed the Run: %#v", after)
		}
	})
}

func TestNestedApprovalResolutionNeverUpdatesRun(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "one"}
	call := &ActionCall{
		ID: "nested", Scope: scope, RunID: "run", TurnID: "step-1", DeploymentID: "agent", SkillID: "repo", SkillVersion: "1.0.0", Action: "push",
		Origin: ActionOriginVM, Status: ActionCallStatusReady, ApprovalID: "approval-1", MaxAttempts: 1, Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	approval := &ApprovalCheckpoint{
		ID: "approval-1", Scope: scope, RunID: "run", ActionCallID: call.ID, Status: ApprovalStatusApproved, Summary: "Push",
		EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "user"}}, ExpiresAt: now.Add(time.Hour), Revision: 2,
		DecisionID: "decision", DecisionBy: &ApprovalPrincipal{Type: "user", ID: "user"}, DecidedAt: &now,
	}
	event := &ActivityEvent{ID: "event", Scope: scope, EventType: "approval.approved", Severity: ActivitySeverityInfo, RunID: "run",
		Actor: ActivityActor{Type: "user", ID: "user"}, Summary: "Approved", Visibility: ActivityVisibilityScope, CreatedAt: now}
	run := &AgentRun{ID: "run", Scope: scope}
	if err := validateApprovalResolutionRecord(ApprovalResolutionRecord{Approval: approval, Call: call, Run: run, Event: event}); err == nil {
		t.Fatal("a nested call's approval resolution updated its Run")
	}
	if err := validateApprovalResolutionRecord(ApprovalResolutionRecord{Approval: approval, Call: call, Event: event}); err != nil {
		t.Fatalf("nested resolution without Run = %v", err)
	}
}

func TestClaimSelectsLoopCalls(t *testing.T) {
	now := time.Now().UTC()
	claim := ActionClaim{Now: now}
	call := &ActionCall{ID: "call", Execution: ActionExecutionLoop, Status: ActionCallStatusReady, AvailableAt: now}
	for status, want := range map[AgentRunStatus]bool{
		AgentRunStatusQueued: false, AgentRunStatusRunning: false, AgentRunStatusPaused: false, AgentRunStatusSleeping: false,
		AgentRunStatusWaitingForApproval: false, AgentRunStatusWaitingForEvent: false, AgentRunStatusWaitingForDependency: false,
		AgentRunStatusCompleted: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true,
	} {
		if got := claim.claimSelects(call, &AgentRun{Status: status}); got != want {
			t.Errorf("pool claim of a loop call with Run %s = %v, want %v", status, got, want)
		}
	}
	if !claim.claimSelects(call, nil) {
		t.Error("pool claim of a loop call without its Run must close it")
	}
	own := ActionClaim{Now: now, OwnActionID: "call"}
	if !own.claimSelects(call, &AgentRun{Status: AgentRunStatusRunning}) {
		t.Error("own claim of a loop call was refused")
	}
	if own.claimSelects(&ActionCall{ID: "call", Status: ActionCallStatusReady}, nil) {
		t.Error("own claim took a worker-executed call")
	}
	if !claim.claimSelects(&ActionCall{ID: "worker", Status: ActionCallStatusReady}, &AgentRun{Status: AgentRunStatusRunning}) {
		t.Error("pool claim of a worker-executed call was refused")
	}
}
