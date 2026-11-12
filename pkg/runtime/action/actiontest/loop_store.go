package actiontest

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// RunLoopStoreScenario runs scenario on SQLite, and on Postgres
// when OPENSEAL_TEST_POSTGRES_DSN is set (Postgres-only checks live in
// runtime's action_loop_postgres_integration_test.go).
func RunLoopStoreScenario(t *testing.T, scenario func(*testing.T, models.KernelStore)) {
	t.Helper()
	for _, backend := range Stores(t) {
		t.Run(backend.Name, func(t *testing.T) { scenario(t, backend.Open(t)) })
	}
}

// LoopStore is one running Run claimed by LoopStoreWorker under a live
// lease, whose step "step-1" proposes loop calls.
type LoopStore struct {
	T     *testing.T
	Ctx   context.Context
	Store models.KernelStore
	Scope models.Scope
	Now   time.Time
	Run   *models.AgentRun
}

// LoopCall is a ready loop call of step-1 with its own idempotency key.
func (f *LoopStore) LoopCall(id string) *models.ActionCall {
	call := &models.ActionCall{
		ID: id, Scope: f.Scope, RunID: f.Run.ID, TurnID: "step-1", DeploymentID: "agent",
		SkillID: "repo", SkillVersion: "1.0.0", Action: "read_file", Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		Status: models.ActionCallStatusReady, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
		Arguments: map[string]interface{}{"path": id}, IdempotencyKey: "tool-call:" + id,
		MaxAttempts: 1, AvailableAt: f.Now, Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now,
	}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	return call
}

// Event is an activity event of the Run by the loop worker.
func (f *LoopStore) Event(id, eventType string) *models.ActivityEvent {
	return &models.ActivityEvent{
		ID: id, Scope: f.Scope, EventType: eventType, Severity: models.ActivitySeverityInfo, AgentID: "agent", RunID: f.Run.ID,
		Actor: models.ActivityActor{Type: "worker", ID: LoopStoreWorker}, Summary: "Loop call", Visibility: models.ActivityVisibilityScope, CreatedAt: f.Now,
	}
}

// Proposal is call's loop proposal without a Run change, fenced by the live
// Run lease.
func (f *LoopStore) Proposal(call *models.ActionCall) models.ActionProposalRecord {
	return models.ActionProposalRecord{
		Call: call, ExpectedRunRevision: f.Run.Revision, Lease: &models.AgentRunLeaseGuard{WorkerID: LoopStoreWorker, Now: f.Now},
		Event: f.Event("event-"+call.ID, "action.proposed"),
	}
}

// Propose stores call's loop proposal and returns the stored call.
func (f *LoopStore) Propose(call *models.ActionCall) *models.ActionCall {
	f.T.Helper()
	result, err := f.Store.CreateActionProposal(f.Ctx, f.Proposal(call))
	if err != nil || result == nil || !result.Created {
		f.T.Fatalf("propose %s = %#v, %v", call.ID, result, err)
	}
	return result.Call
}

// CurrentRun reads the Run as stored.
func (f *LoopStore) CurrentRun() *models.AgentRun {
	f.T.Helper()
	run, err := f.Store.GetAgentRun(f.Ctx, f.Scope, f.Run.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	return run
}

// SetRunStatus moves the Run to status outside any lease, as a park,
// requeue, pause or cancel would.
func (f *LoopStore) SetRunStatus(status models.AgentRunStatus) {
	f.T.Helper()
	current := f.CurrentRun()
	updated := models.CloneAgentRun(current)
	updated.Status = status
	updated.Revision++
	updated.UpdatedAt = f.Now
	if status != models.AgentRunStatusRunning {
		updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	}
	if models.IsTerminalAgentRunStatus(status) {
		updated.CompletedAt = &f.Now
	}
	if _, err := f.Store.UpdateAgentRunWithEvent(f.Ctx, updated, current.Revision, f.Event("status-"+string(status)+"-"+uuid.NewString()[:8], "run.status_changed"), nil); err != nil {
		f.T.Fatalf("set Run %s: %v", status, err)
	}
	f.Run = f.CurrentRun()
}

// PoolClaim claims the next action at at as the pool worker does.
func (f *LoopStore) PoolClaim(at time.Time) *models.ActionCall {
	f.T.Helper()
	claimed, err := f.Store.ClaimNextAction(f.Ctx, models.ActionClaim{Scope: f.Scope, WorkerID: "pool-worker", Now: at, LeaseDuration: time.Minute})
	if err != nil {
		f.T.Fatal(err)
	}
	return claimed
}

// OwnClaim claims call id at at as the loop worker claims its own call.
func (f *LoopStore) OwnClaim(id string, at time.Time) *models.ActionCall {
	f.T.Helper()
	claimed, err := f.Store.ClaimNextAction(f.Ctx, models.ActionClaim{Scope: f.Scope, WorkerID: LoopStoreWorker, Now: at, LeaseDuration: models.LoopActionLease, OwnActionID: id})
	if err != nil {
		f.T.Fatal(err)
	}
	return claimed
}

// RequireNoCall fails the test when call id was stored.
func (f *LoopStore) RequireNoCall(id string) {
	f.T.Helper()
	if call, err := f.Store.GetActionCall(f.Ctx, f.Scope, id); !errors.Is(err, models.ErrActionNotFound) {
		f.T.Fatalf("call %s = %#v, %v; want none", id, call, err)
	}
}

// ProposeLoopApproval commits a write loop call of step-1 waiting on its
// approval, with the Run unchanged unless the call reserves budget.
func (f *LoopStore) ProposeLoopApproval(id string, opts LoopApprovalOptions) (*models.ActionCall, *models.ApprovalCheckpoint) {
	f.T.Helper()
	call := f.LoopCall(id)
	call.Action, call.Risk, call.SideEffect = "push", skill.RiskLevelExternal, skill.SideEffectExternal
	call.Status, call.ApprovalID = models.ActionCallStatusWaitingApproval, "approval-"+id
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	proposed := map[string]interface{}{"skillId": call.SkillID, "action": call.Action, "arguments": maps.Clone(call.Arguments)}
	if opts.AlwaysReview {
		proposed["review"] = "always"
	}
	approval := &models.ApprovalCheckpoint{
		ID: call.ApprovalID, Scope: f.Scope, RunID: f.Run.ID, ActionCallID: call.ID, Status: models.ApprovalStatusPending,
		Risk: skill.RiskLevelExternal, Summary: "Push the fix to the release branch", ProposedAction: proposed,
		EligibleApprovers: []models.ApprovalPrincipal{LoopApprover}, TimeoutDecision: opts.Timeout,
		ExpiresAt: f.Now.Add(time.Hour), Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now,
	}
	proposal := f.Proposal(call)
	proposal.Approval = approval
	if opts.ReserveBudget {
		current := f.CurrentRun()
		reserved := models.CloneAgentRun(current)
		if err := models.ReserveRunBudget(reserved, models.BudgetReservation{ID: agentrun.ActionBudgetReservationID(call.ID), Usage: models.BudgetUsage{Actions: 1}, CreatedAt: f.Now}); err != nil {
			f.T.Fatal(err)
		}
		reserved.Revision++
		proposal.Run, proposal.ExpectedRunRevision = reserved, current.Revision
	}
	result, err := f.Store.CreateActionProposal(f.Ctx, proposal)
	if err != nil || !result.Created {
		f.T.Fatalf("propose %s = %#v, %v", id, result, err)
	}
	f.Run = f.CurrentRun()
	return result.Call, result.Approval
}

// ParkOn moves the running Run to waiting_for_approval on approvalID, as the
// loop's step-end park does.
func (f *LoopStore) ParkOn(approvalID string) {
	f.T.Helper()
	current := f.CurrentRun()
	parked := models.CloneAgentRun(current)
	parked.Status = models.AgentRunStatusWaitingForApproval
	parked.WakeCondition = &models.WakeCondition{Type: "approval", Reference: approvalID}
	parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
	parked.Revision++
	parked.UpdatedAt = f.Now
	if _, err := f.Store.UpdateAgentRunWithEvent(f.Ctx, parked, current.Revision, f.Event("park-"+uuid.NewString()[:8], "run.waiting"), nil); err != nil {
		f.T.Fatalf("park on %s: %v", approvalID, err)
	}
	f.Run = f.CurrentRun()
}

// ApprovalCoordinator is an approval coordinator whose clock reads at.
func (f *LoopStore) ApprovalCoordinator(at time.Time) *action.ApprovalCoordinator {
	coordinator := action.NewApprovalCoordinator(f.Store, f.Store, action.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return at }))
	return coordinator
}

// Decide resolves approval with decision by LoopApprover through coordinator.
func (f *LoopStore) Decide(coordinator *action.ApprovalCoordinator, approval *models.ApprovalCheckpoint, decision models.ApprovalDecision, reason string) (*models.ApprovalResolutionResult, error) {
	return coordinator.Resolve(f.Ctx, action.ResolveApprovalRequest{
		Scope: f.Scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "decision-" + approval.ID, Decision: decision, Principal: LoopApprover, Reason: reason,
	})
}

// NewLoopStore creates a Run on store and claims it as LoopStoreWorker.
func NewLoopStore(t *testing.T, store models.KernelStore) *LoopStore {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "loop-" + uuid.NewString()[:8]}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := portfolio.NewService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "check the repo", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: LoopStoreWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	return &LoopStore{T: t, Ctx: ctx, Store: store, Scope: scope, Now: now, Run: run}
}

// LoopApprover is the person who decides the fixture's approvals.
var LoopApprover = models.ApprovalPrincipal{Type: "user", ID: "user"}

// LoopStoreWorker holds the lease of the fixture's Run.
const LoopStoreWorker = "loop-worker"

// LoopApprovalOptions shape the approval ProposeLoopApproval creates.
type LoopApprovalOptions struct {
	ReserveBudget bool
	Timeout       models.ApprovalTimeoutDecision
	AlwaysReview  bool
}

// ResolveWith resolves approval with decision by principal through an
// approval coordinator that authorizes with authorizer.
func (f *LoopStore) ResolveWith(authorizer action.ApprovalAuthorizer, approval *models.ApprovalCheckpoint, decision models.ApprovalDecision, principal models.ApprovalPrincipal, reason string) (*models.ApprovalResolutionResult, error) {
	coordinator := action.NewApprovalCoordinator(f.Store, f.Store, authorizer, models.WithClock(func() time.Time { return f.Now.Add(time.Second) }))
	return coordinator.Resolve(f.Ctx, action.ResolveApprovalRequest{
		Scope: f.Scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "decision-" + approval.ID, Decision: decision, Principal: principal, Reason: reason,
	})
}
