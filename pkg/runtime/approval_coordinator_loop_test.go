package runtime

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

var loopApprover = ApprovalPrincipal{Type: "user", ID: "user"}

// newLoopApprovalFixture is newLoopStoreFixture with an optional Run budget.
func newLoopApprovalFixture(t *testing.T, store KernelStore, budget *BudgetPolicy) *loopStoreFixture {
	t.Helper()
	if budget == nil {
		return newLoopStoreFixture(t, store)
	}
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "loop-" + uuid.NewString()[:8]}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "push the fix", Source: RunSourceObjective,
		Budget: budget,
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

type loopApprovalOptions struct {
	reserveBudget bool
	timeout       ApprovalTimeoutDecision
	alwaysReview  bool
}

// proposeLoopApproval commits a write loop call of step-1 waiting on its
// approval, with the Run unchanged unless the call reserves budget.
func (f *loopStoreFixture) proposeLoopApproval(id string, opts loopApprovalOptions) (*ActionCall, *ApprovalCheckpoint) {
	f.t.Helper()
	call := f.loopCall(id)
	call.Action, call.Risk, call.SideEffect = "push", skill.RiskLevelExternal, skill.SideEffectExternal
	call.Status, call.ApprovalID = ActionCallStatusWaitingApproval, "approval-"+id
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	proposed := map[string]interface{}{"skillId": call.SkillID, "action": call.Action, "arguments": maps.Clone(call.Arguments)}
	if opts.alwaysReview {
		proposed["review"] = "always"
	}
	approval := &ApprovalCheckpoint{
		ID: call.ApprovalID, Scope: f.scope, RunID: f.run.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
		Risk: skill.RiskLevelExternal, Summary: "Push the fix to the release branch", ProposedAction: proposed,
		EligibleApprovers: []ApprovalPrincipal{loopApprover}, TimeoutDecision: opts.timeout,
		ExpiresAt: f.now.Add(time.Hour), Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	proposal := f.proposal(call)
	proposal.Approval = approval
	if opts.reserveBudget {
		current := f.currentRun()
		reserved := cloneAgentRun(current)
		if err := reserveRunBudget(reserved, BudgetReservation{ID: agentrun.ActionBudgetReservationID(call.ID), Usage: BudgetUsage{Actions: 1}, CreatedAt: f.now}); err != nil {
			f.t.Fatal(err)
		}
		reserved.Revision++
		proposal.Run, proposal.ExpectedRunRevision = reserved, current.Revision
	}
	result, err := f.store.CreateActionProposal(f.ctx, proposal)
	if err != nil || !result.Created {
		f.t.Fatalf("propose %s = %#v, %v", id, result, err)
	}
	f.run = f.currentRun()
	return result.Call, result.Approval
}

// parkOn moves the running Run to waiting_for_approval on approvalID, as the
// loop's step-end park does.
func (f *loopStoreFixture) parkOn(approvalID string) {
	f.t.Helper()
	current := f.currentRun()
	parked := cloneAgentRun(current)
	parked.Status = AgentRunStatusWaitingForApproval
	parked.WakeCondition = &WakeCondition{Type: "approval", Reference: approvalID}
	parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
	parked.Revision++
	parked.UpdatedAt = f.now
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, parked, current.Revision, f.event("park-"+uuid.NewString()[:8], "run.waiting"), nil); err != nil {
		f.t.Fatalf("park on %s: %v", approvalID, err)
	}
	f.run = f.currentRun()
}

func (f *loopStoreFixture) approvalCoordinator(at time.Time) *ApprovalCoordinator {
	coordinator := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return at }))
	return coordinator
}

func (f *loopStoreFixture) decide(coordinator *ApprovalCoordinator, approval *ApprovalCheckpoint, decision ApprovalDecision, reason string) (*ApprovalResolutionResult, error) {
	return coordinator.Resolve(f.ctx, ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "decision-" + approval.ID, Decision: decision, Principal: loopApprover, Reason: reason,
	})
}

func requireQueuedAfterDecision(t *testing.T, run *AgentRun, decisionID string, at time.Time) {
	t.Helper()
	if run.Status != AgentRunStatusQueued || run.WakeCondition != nil || run.LeaseOwner != "" || run.LeaseExpiresAt != nil ||
		!run.AvailableAt.Equal(at) || !run.QueueEnteredAt.Equal(at) || run.LastWakeSignalID != decisionID {
		t.Fatalf("Run after parked decision = %#v", run)
	}
}

func TestLoopParkedApprovalApprovedQueuesRunAndReadiesCall(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		call, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
		f.parkOn(approval.ID)
		parkedRevision := f.run.Revision
		at := f.now.Add(2 * time.Second)
		result, err := f.decide(f.approvalCoordinator(at), approval, ApprovalDecisionApprove, "")
		if err != nil {
			t.Fatal(err)
		}
		if !result.Resolved || result.Approval.Status != ApprovalStatusApproved || result.Call.Status != ActionCallStatusReady || result.Call.ID != call.ID {
			t.Fatalf("resolution = %#v", result)
		}
		run := f.currentRun()
		requireQueuedAfterDecision(t, run, "decision-"+approval.ID, at)
		if run.Revision != parkedRevision+1 {
			t.Fatalf("Run revision = %d, want %d", run.Revision, parkedRevision+1)
		}
		claimed, err := store.ClaimNextAgentRun(f.ctx, AgentRunClaim{Scope: f.scope, WorkerID: "next-worker", Now: at.Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("the approved Run is not claimable: %#v, %v", claimed, err)
		}
		if replay, err := f.decide(f.approvalCoordinator(at), approval, ApprovalDecisionApprove, ""); err != nil || replay.Resolved {
			t.Fatalf("replay = %#v, %v", replay, err)
		}
		if _, err := f.approvalCoordinator(at).Resolve(f.ctx, ResolveApprovalRequest{
			Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: result.Approval.Revision,
			DecisionID: "other", Decision: ApprovalDecisionReject, Principal: loopApprover,
		}); !errors.Is(err, ErrApprovalResolved) {
			t.Fatalf("second decision = %v", err)
		}
	})
}

func TestLoopParkedApprovalRejectedQueuesRunAndDeniesCall(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopApprovalFixture(t, store, &BudgetPolicy{MaxActions: 2})
		_, approval := f.proposeLoopApproval("push", loopApprovalOptions{reserveBudget: true})
		if len(f.run.BudgetReservations) != 1 {
			t.Fatalf("reservation = %#v", f.run.BudgetReservations)
		}
		f.parkOn(approval.ID)
		at := f.now.Add(2 * time.Second)
		result, err := f.decide(f.approvalCoordinator(at), approval, ApprovalDecisionReject, "not this branch")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusRejected || result.Call.Status != ActionCallStatusDenied ||
			result.Call.Error != "rejected: not this branch" || result.Call.CompletedAt == nil {
			t.Fatalf("rejection = %#v", result)
		}
		run := f.currentRun()
		requireQueuedAfterDecision(t, run, "decision-"+approval.ID, at)
		if len(run.BudgetReservations) != 0 || run.BudgetUsage.Actions != 0 || len(run.Checkpoint) != 0 {
			t.Fatalf("rejected Run budget/checkpoint = %#v", run)
		}
	})
}

func TestLoopParkedApprovalChangesRequestedRequiresGuidance(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
		f.parkOn(approval.ID)
		at := f.now.Add(2 * time.Second)
		if _, err := f.decide(f.approvalCoordinator(at), approval, ApprovalDecisionRequestChanges, " "); err == nil || !strings.Contains(err.Error(), "reviewer guidance is required") {
			t.Fatalf("missing guidance = %v", err)
		}
		if run := f.currentRun(); run.Status != AgentRunStatusWaitingForApproval {
			t.Fatalf("a refused change request moved the Run: %#v", run)
		}
		const guidance = "Push to the staging branch first."
		result, err := f.decide(f.approvalCoordinator(at), approval, ApprovalDecisionRequestChanges, guidance)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusChangesRequested || result.Approval.DecisionReason != guidance ||
			result.Call.Status != ActionCallStatusDenied || result.Call.Error != "changes_requested: "+guidance ||
			result.Event.EventType != "approval.changes_requested" {
			t.Fatalf("change request = %#v", result)
		}
		requireQueuedAfterDecision(t, f.currentRun(), "decision-"+approval.ID, at)
	})
}

func TestLoopParkedApprovalExpiredQueues(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
		f.parkOn(approval.ID)
		early := f.approvalCoordinator(approval.ExpiresAt.Add(-time.Second))
		if _, err := early.ResolveTimeout(f.ctx, f.scope, approval.ID, approval.Revision, approval.ID); err == nil || !strings.Contains(err.Error(), "deadline has not elapsed") {
			t.Fatalf("early timeout = %v", err)
		}
		at := approval.ExpiresAt.Add(time.Second)
		result, err := f.approvalCoordinator(at).ResolveTimeout(f.ctx, f.scope, approval.ID, approval.Revision, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied || result.Event.EventType != "approval.expired" {
			t.Fatalf("expiry = %#v", result)
		}
		requireQueuedAfterDecision(t, f.currentRun(), "approval-timeout:"+approval.ID, at)
	})
}

func TestLoopParkedApprovalTimeoutAutoApproveNeverForAlwaysReview(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, reviewed := f.proposeLoopApproval("always", loopApprovalOptions{timeout: ApprovalTimeoutApprove, alwaysReview: true})
		_, plain := f.proposeLoopApproval("plain", loopApprovalOptions{timeout: ApprovalTimeoutApprove})
		f.parkOn(reviewed.ID)
		at := reviewed.ExpiresAt.Add(time.Second)
		coordinator := f.approvalCoordinator(at)
		result, err := coordinator.ResolveTimeout(f.ctx, f.scope, reviewed.ID, reviewed.Revision, reviewed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied {
			t.Fatalf("an always-review approval auto-approved: %#v", result)
		}
		requireQueuedAfterDecision(t, f.currentRun(), "approval-timeout:"+reviewed.ID, at)

		// The other call is still held by the queued Run: its timeout rule applies.
		before := f.currentRun()
		auto, err := coordinator.ResolveTimeout(f.ctx, f.scope, plain.ID, plain.Revision, plain.ID)
		if err != nil {
			t.Fatal(err)
		}
		if auto.Approval.Status != ApprovalStatusApproved || auto.Call.Status != ActionCallStatusReady || auto.Event.EventType != "approval.auto_approved" {
			t.Fatalf("timeout auto-approve = %#v", auto)
		}
		if after := f.currentRun(); after.Status != AgentRunStatusQueued || after.Revision != before.Revision+1 {
			t.Fatalf("held auto-approve Run = %#v (before revision %d)", after, before.Revision)
		}
	})
}

func TestLoopHeldApprovalDecisionBumpsRunRevision(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopApprovalFixture(t, store, &BudgetPolicy{MaxActions: 3})
		_, approved := f.proposeLoopApproval("approved", loopApprovalOptions{reserveBudget: true})
		_, rejected := f.proposeLoopApproval("rejected", loopApprovalOptions{reserveBudget: true})
		at := f.now.Add(2 * time.Second)
		coordinator := f.approvalCoordinator(at)

		before := f.currentRun()
		result, err := f.decide(coordinator, approved, ApprovalDecisionApprove, "")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusApproved || result.Call.Status != ActionCallStatusReady {
			t.Fatalf("held approve = %#v", result)
		}
		after := f.currentRun()
		if after.Revision != before.Revision+1 || after.Status != AgentRunStatusRunning || after.LeaseOwner != loopStoreWorker ||
			after.LeaseExpiresAt == nil || !after.LeaseExpiresAt.Equal(*before.LeaseExpiresAt) || after.WakeCondition != nil ||
			len(after.BudgetReservations) != 2 || after.LastWakeSignalID != before.LastWakeSignalID {
			t.Fatalf("held approve Run = %#v (before %#v)", after, before)
		}

		result, err = f.decide(coordinator, rejected, ApprovalDecisionReject, "no")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusRejected || result.Call.Status != ActionCallStatusDenied {
			t.Fatalf("held reject = %#v", result)
		}
		final := f.currentRun()
		if final.Revision != after.Revision+1 || final.Status != AgentRunStatusRunning || final.LeaseOwner != loopStoreWorker {
			t.Fatalf("held reject Run = %#v", final)
		}
		if _, still := final.BudgetReservations[agentrun.ActionBudgetReservationID("rejected")]; still || len(final.BudgetReservations) != 1 {
			t.Fatalf("held reject kept its reservation: %#v", final.BudgetReservations)
		}
	})
}

// A park of the step planned before a held decision carries the Run
// revision it read; the decision's bump makes that park fail its check, so
// the loop re-plans and sees the decision.
func TestLoopHeldApprovalDecisionFailsStalePark(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
		planned := f.currentRun()
		if _, err := f.decide(f.approvalCoordinator(f.now.Add(time.Second)), approval, ApprovalDecisionApprove, ""); err != nil {
			t.Fatal(err)
		}
		parked := cloneAgentRun(planned)
		parked.Status = AgentRunStatusWaitingForApproval
		parked.WakeCondition = &WakeCondition{Type: "approval", Reference: approval.ID}
		parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
		parked.Revision++
		if _, err := store.UpdateAgentRunWithEvent(f.ctx, parked, planned.Revision, f.event("stale-park", "run.waiting"), nil); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("stale park = %v", err)
		}
		if run := f.currentRun(); run.Status != AgentRunStatusRunning {
			t.Fatalf("Run = %#v", run)
		}
	})
}

func TestLoopHeldApprovalApprovalModeSweep(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
		before := f.currentRun()
		result, err := f.approvalCoordinator(f.now.Add(time.Second)).ResolveByApprovalMode(f.ctx, f.scope, approval.ID, approval.Revision, ConversationApprovalSkip, "mode-change")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusApproved || result.Approval.DecisionBy == nil || *result.Approval.DecisionBy != ApprovalModePrincipal ||
			result.Call.Status != ActionCallStatusReady {
			t.Fatalf("mode sweep = %#v", result)
		}
		if after := f.currentRun(); after.Status != AgentRunStatusRunning || after.Revision != before.Revision+1 || after.LeaseOwner != loopStoreWorker {
			t.Fatalf("mode sweep Run = %#v", after)
		}
	})
}

func TestLoopSupersededRecordsMessageID(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, held := f.proposeLoopApproval("held", loopApprovalOptions{})
		_, parked := f.proposeLoopApproval("parked", loopApprovalOptions{})
		at := f.now.Add(time.Second)
		coordinator := f.approvalCoordinator(at)
		before := f.currentRun()
		result, err := coordinator.ResolveSupersededByMessage(f.ctx, f.scope, held.ID, held.Revision, "message-1")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusSuperseded || result.Approval.SupersededByMessageID != "message-1" ||
			result.Call.Status != ActionCallStatusDenied || result.Call.Error != "superseded: "+ApprovalSupersededReason {
			t.Fatalf("held supersede = %#v", result)
		}
		if after := f.currentRun(); after.Status != AgentRunStatusRunning || after.Revision != before.Revision+1 {
			t.Fatalf("held supersede Run = %#v", after)
		}
		stored, err := store.GetApproval(f.ctx, f.scope, held.ID)
		if err != nil || stored.SupersededByMessageID != "message-1" {
			t.Fatalf("stored approval = %#v, %v", stored, err)
		}

		f.parkOn(parked.ID)
		result, err = coordinator.ResolveSupersededByMessage(f.ctx, f.scope, parked.ID, parked.Revision, "message-2")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.SupersededByMessageID != "message-2" || result.Event.EventType != "approval.superseded" || result.Event.CorrelationID != "message-2" {
			t.Fatalf("parked supersede = %#v", result)
		}
		requireQueuedAfterDecision(t, f.currentRun(), "superseded:"+parked.ID, at)
	})
}

func TestLoopPausedParkedApprovalResolvesToPausedQueued(t *testing.T) {
	for _, decision := range []ApprovalDecision{ApprovalDecisionApprove, ApprovalDecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
				f := newLoopStoreFixture(t, store)
				_, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
				f.parkOn(approval.ID)
				paused, err := agentrun.NewRunCommandService(store).CommandAgentRun(f.ctx, agentrun.AgentRunCommandRequest{
					Scope: f.scope, RunID: f.run.ID, ExpectedRevision: f.run.Revision, Kind: agentrun.AgentRunCommandPause,
					Actor: ActivityActor{Type: "user", ID: "operator"},
				})
				if err != nil || paused.Run.Status != AgentRunStatusPaused || paused.Run.PausedFrom != AgentRunStatusWaitingForApproval {
					t.Fatalf("pause = %#v, %v", paused, err)
				}
				result, err := f.decide(f.approvalCoordinator(f.now.Add(time.Second)), approval, decision, "reason")
				if err != nil {
					t.Fatal(err)
				}
				want := ActionCallStatusReady
				if decision == ApprovalDecisionReject {
					want = ActionCallStatusDenied
				}
				if result.Call.Status != want {
					t.Fatalf("call = %#v", result.Call)
				}
				run := f.currentRun()
				if run.Status != AgentRunStatusPaused || run.PausedFrom != AgentRunStatusQueued || run.PausedWakeCondition != nil ||
					run.Revision != paused.Run.Revision+1 {
					t.Fatalf("paused Run = %#v", run)
				}
			})
		})
	}
}

func TestLoopApprovalOfEndedRun(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
		f.setRunStatus(AgentRunStatusFailed)
		ended := f.currentRun()
		if _, err := f.decide(f.approvalCoordinator(f.now.Add(time.Second)), approval, ApprovalDecisionApprove, ""); !errors.Is(err, ErrInvalidRunTransition) {
			t.Fatalf("decision on an ended Run = %v", err)
		}
		result, err := f.approvalCoordinator(approval.ExpiresAt.Add(time.Second)).ResolveTimeout(f.ctx, f.scope, approval.ID, approval.Revision, approval.ID)
		if err != nil || result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied {
			t.Fatalf("expiry of an ended Run's approval = %#v, %v", result, err)
		}
		if run := f.currentRun(); run.Revision != ended.Revision || run.Status != AgentRunStatusFailed {
			t.Fatalf("expiry touched the ended Run: %#v", run)
		}
	})
}

// raceRunPortfolio bumps the Run revision with a lease heartbeat right after
// the coordinator reads the Run, the first `races` times.
type raceRunPortfolio struct {
	PortfolioStore
	store KernelStore
	races atomic.Int32
	now   time.Time
}

func (p *raceRunPortfolio) GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error) {
	run, err := p.PortfolioStore.GetAgentRun(ctx, scope, runID)
	if err == nil && p.races.Add(-1) >= 0 {
		if _, renewErr := p.store.RenewAgentRunLease(ctx, scope, runID, loopStoreWorker, p.now, time.Hour); renewErr != nil {
			return nil, renewErr
		}
	}
	return run, err
}

func TestLoopHeldApprovalRetriesWhenRunMoves(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, approval := f.proposeLoopApproval("push", loopApprovalOptions{})
		_, starved := f.proposeLoopApproval("starved", loopApprovalOptions{})
		at := f.now.Add(time.Second)
		portfolio := &raceRunPortfolio{PortfolioStore: store, store: store, now: at}
		portfolio.races.Store(approvalResolveAttempts - 1)
		coordinator := NewApprovalCoordinator(portfolio, store, EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return at }))
		result, err := f.decide(coordinator, approval, ApprovalDecisionApprove, "")
		if err != nil || !result.Resolved || result.Call.Status != ActionCallStatusReady {
			t.Fatalf("decision after racing heartbeats = %#v, %v", result, err)
		}
		portfolio.races.Store(approvalResolveAttempts)
		if _, err := f.decide(coordinator, starved, ApprovalDecisionApprove, ""); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("decision losing every race = %v", err)
		}
		if stored, err := store.GetApproval(f.ctx, f.scope, starved.ID); err != nil || stored.Status != ApprovalStatusPending {
			t.Fatalf("starved approval = %#v, %v", stored, err)
		}
	})
}
