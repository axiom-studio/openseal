package actiontest

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/google/uuid"
)

// LoopProposalStep is the step that proposes the governed tool calls of a
// LoopProposalFixture.
const LoopProposalStep = "step-1"

// LoopProposalFixture holds a Run the way the run loop holds it while one of
// its steps proposes governed tool calls: running, leased by worker for an
// hour. Proposals leave the Run unchanged (apart from a budget reservation);
// an approval or card parks it only at step end (Park).
type LoopProposalFixture struct {
	t      *testing.T
	Store  models.KernelStore
	Worker string
	Now    time.Time
	Run    *models.AgentRun
}

// NewLoopProposalFixture leases run to worker at now for an hour and moves it
// to running, as the loop's claim does. run may be in any non-terminal status
// (a queued Run, a Run claimed by another worker, or a running Run without a
// lease).
func NewLoopProposalFixture(t *testing.T, store models.KernelStore, run *models.AgentRun, worker string, now time.Time) *LoopProposalFixture {
	t.Helper()
	f := &LoopProposalFixture{t: t, Store: store, Worker: worker, Now: now, Run: run}
	current := f.Current()
	leased := models.CloneAgentRun(current)
	expires := now.Add(time.Hour)
	leased.Status, leased.LeaseOwner, leased.LeaseExpiresAt, leased.WakeCondition = models.AgentRunStatusRunning, worker, &expires, nil
	leased.Revision++
	leased.UpdatedAt = now
	if _, err := store.UpdateAgentRunWithEvent(context.Background(), leased, current.Revision, f.Event("lease"), nil); err != nil {
		t.Fatalf("lease Run %s to %s: %v", run.ID, worker, err)
	}
	f.Run = f.Current()
	return f
}

// Request is req as this fixture's step proposes tool call toolCallID: the
// fixture's Run, scope and lease owner unless req names them, loop origin and
// execution, and the per-call idempotency key unless req has one.
func (f *LoopProposalFixture) Request(req action.ProposeActionRequest, toolCallID string) action.ProposeActionRequest {
	if req.Scope == (models.Scope{}) {
		req.Scope = f.Run.Scope
	}
	if req.RunID == "" {
		req.RunID = f.Run.ID
	}
	if req.WorkerID == "" {
		req.WorkerID = f.Worker
	}
	if req.TurnID == "" {
		req.TurnID = LoopProposalStep
	}
	return LoopProposeRequest(req, toolCallID)
}

func (f *LoopProposalFixture) Current() *models.AgentRun {
	f.t.Helper()
	run, err := f.Store.GetAgentRun(context.Background(), f.Run.Scope, f.Run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("get Run %s = %#v, %v", f.Run.ID, run, err)
	}
	return run
}

// AssertUntouched checks the proposals so far left the Run as leased.
func (f *LoopProposalFixture) AssertUntouched() {
	f.t.Helper()
	AssertRunUntouched(f.t, f.Store, f.Run)
}

// Park moves the running Run to status waiting on wake, releasing its lease,
// as the loop's step-end park does.
func (f *LoopProposalFixture) Park(status models.AgentRunStatus, wake *models.WakeCondition) {
	f.t.Helper()
	current := f.Current()
	parked := models.CloneAgentRun(current)
	parked.Status, parked.WakeCondition = status, wake
	parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
	parked.Revision++
	parked.UpdatedAt = f.Now
	if _, err := f.Store.UpdateAgentRunWithEvent(context.Background(), parked, current.Revision, f.Event("park"), nil); err != nil {
		f.t.Fatalf("park Run %s: %v", current.ID, err)
	}
	f.Run = f.Current()
}

// ParkOnApproval parks the Run waiting for approvalID.
func (f *LoopProposalFixture) ParkOnApproval(approvalID string) {
	f.Park(models.AgentRunStatusWaitingForApproval, &models.WakeCondition{Type: "approval", Reference: approvalID})
}

// Execute runs call inline for the Run's lease owner, as the loop's governed
// executor does after a ready proposal.
func (f *LoopProposalFixture) Execute(worker *action.ActionWorker, callID string) *models.ActionExecutionResult {
	f.t.Helper()
	result, err := worker.ExecuteOwnLoopCall(context.Background(), f.Run.Scope, callID, f.Worker)
	if err != nil || result == nil {
		f.t.Fatalf("execute loop call %s = %#v, %v", callID, result, err)
	}
	return result
}

func (f *LoopProposalFixture) Event(kind string) *models.ActivityEvent {
	return &models.ActivityEvent{
		ID: kind + "-" + uuid.NewString()[:8], Scope: f.Run.Scope, EventType: "run.updated", Severity: models.ActivitySeverityInfo,
		AgentID: f.Run.AssignedAgentID, RunID: f.Run.ID, Actor: models.ActivityActor{Type: "worker", ID: f.Worker},
		Summary: "Run loop " + kind, Visibility: models.ActivityVisibilityScope, CreatedAt: f.Now,
	}
}

// AsLoopCall makes call a loop call of LoopProposalStep and refreshes its
// digests.
func AsLoopCall(call *models.ActionCall) *models.ActionCall {
	call.Origin, call.Execution = models.ActionOriginLoop, models.ActionExecutionLoop
	if call.TurnID == "" {
		call.TurnID = LoopProposalStep
	}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	return call
}

// LoopProposalRecord is call's loop proposal as ActionCoordinator.Propose
// records it: the Run is unchanged, fenced by worker's live lease at now.
func LoopProposalRecord(call *models.ActionCall, run *models.AgentRun, worker string, now time.Time, event *models.ActivityEvent) models.ActionProposalRecord {
	return models.ActionProposalRecord{
		Call: AsLoopCall(call), ExpectedRunRevision: run.Revision,
		Lease: &models.AgentRunLeaseGuard{WorkerID: worker, Now: now}, Event: event,
	}
}
