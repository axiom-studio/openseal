package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// loopProposalStep is the step that proposes the governed tool calls of a
// loopProposalFixture.
const loopProposalStep = "step-1"

// loopProposalFixture holds a Run the way the run loop holds it while one of
// its steps proposes governed tool calls: running, leased by worker for an
// hour. Proposals leave the Run unchanged (apart from a budget reservation);
// an approval or card parks it only at step end (park).
type loopProposalFixture struct {
	t      *testing.T
	store  KernelStore
	worker string
	now    time.Time
	run    *AgentRun
}

// newLoopProposalFixture leases run to worker at now for an hour and moves it
// to running, as the loop's claim does. run may be in any non-terminal status
// (a queued Run, a Run claimed by another worker, or a running Run without a
// lease).
func newLoopProposalFixture(t *testing.T, store KernelStore, run *AgentRun, worker string, now time.Time) *loopProposalFixture {
	t.Helper()
	f := &loopProposalFixture{t: t, store: store, worker: worker, now: now, run: run}
	current := f.current()
	leased := cloneAgentRun(current)
	expires := now.Add(time.Hour)
	leased.Status, leased.LeaseOwner, leased.LeaseExpiresAt, leased.WakeCondition = AgentRunStatusRunning, worker, &expires, nil
	leased.Revision++
	leased.UpdatedAt = now
	if _, err := store.UpdateAgentRunWithEvent(context.Background(), leased, current.Revision, f.event("lease"), nil); err != nil {
		t.Fatalf("lease Run %s to %s: %v", run.ID, worker, err)
	}
	f.run = f.current()
	return f
}

// request is req as this fixture's step proposes tool call toolCallID: the
// fixture's Run, scope and lease owner unless req names them, loop origin and
// execution, and the per-call idempotency key unless req has one.
func (f *loopProposalFixture) request(req ProposeActionRequest, toolCallID string) ProposeActionRequest {
	if req.Scope == (Scope{}) {
		req.Scope = f.run.Scope
	}
	if req.RunID == "" {
		req.RunID = f.run.ID
	}
	if req.WorkerID == "" {
		req.WorkerID = f.worker
	}
	if req.TurnID == "" {
		req.TurnID = loopProposalStep
	}
	return loopProposeRequest(req, toolCallID)
}

func (f *loopProposalFixture) current() *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(context.Background(), f.run.Scope, f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("get Run %s = %#v, %v", f.run.ID, run, err)
	}
	return run
}

// assertUntouched checks the proposals so far left the Run as leased.
func (f *loopProposalFixture) assertUntouched() {
	f.t.Helper()
	assertLoopRunUntouched(f.t, f.store, f.run)
}

// park moves the running Run to status waiting on wake, releasing its lease,
// as the loop's step-end park does.
func (f *loopProposalFixture) park(status AgentRunStatus, wake *WakeCondition) {
	f.t.Helper()
	current := f.current()
	parked := cloneAgentRun(current)
	parked.Status, parked.WakeCondition = status, wake
	parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
	parked.Revision++
	parked.UpdatedAt = f.now
	if _, err := f.store.UpdateAgentRunWithEvent(context.Background(), parked, current.Revision, f.event("park"), nil); err != nil {
		f.t.Fatalf("park Run %s: %v", current.ID, err)
	}
	f.run = f.current()
}

// parkOnApproval parks the Run waiting for approvalID.
func (f *loopProposalFixture) parkOnApproval(approvalID string) {
	f.park(AgentRunStatusWaitingForApproval, &WakeCondition{Type: "approval", Reference: approvalID})
}

// execute runs call inline for the Run's lease owner, as the loop's governed
// executor does after a ready proposal.
func (f *loopProposalFixture) execute(worker *ActionWorker, callID string) *ActionExecutionResult {
	f.t.Helper()
	result, err := worker.ExecuteOwnLoopCall(context.Background(), f.run.Scope, callID, f.worker)
	if err != nil || result == nil {
		f.t.Fatalf("execute loop call %s = %#v, %v", callID, result, err)
	}
	return result
}

func (f *loopProposalFixture) event(kind string) *ActivityEvent {
	return &ActivityEvent{
		ID: kind + "-" + uuid.NewString()[:8], Scope: f.run.Scope, EventType: "run.updated", Severity: ActivitySeverityInfo,
		AgentID: f.run.AssignedAgentID, RunID: f.run.ID, Actor: ActivityActor{Type: "worker", ID: f.worker},
		Summary: "Run loop " + kind, Visibility: ActivityVisibilityScope, CreatedAt: f.now,
	}
}

// asLoopCall makes call a loop call of loopProposalStep and refreshes its
// digests.
func asLoopCall(call *ActionCall) *ActionCall {
	call.Origin, call.Execution = ActionOriginLoop, ActionExecutionLoop
	if call.TurnID == "" {
		call.TurnID = loopProposalStep
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	return call
}

// loopProposalRecord is call's loop proposal as ActionCoordinator.Propose
// records it: the Run is unchanged, fenced by worker's live lease at now.
func loopProposalRecord(call *ActionCall, run *AgentRun, worker string, now time.Time, event *ActivityEvent) ActionProposalRecord {
	return ActionProposalRecord{
		Call: asLoopCall(call), ExpectedRunRevision: run.Revision,
		Lease: &AgentRunLeaseGuard{WorkerID: worker, Now: now}, Event: event,
	}
}
