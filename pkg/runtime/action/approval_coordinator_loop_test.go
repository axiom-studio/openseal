package action_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/google/uuid"
)

// newLoopApprovalFixture is actiontest.NewLoopStore with an optional Run budget.
func newLoopApprovalFixture(t *testing.T, store models.KernelStore, budget *models.BudgetPolicy) *actiontest.LoopStore {
	t.Helper()
	if budget == nil {
		return actiontest.NewLoopStore(t, store)
	}
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "loop-" + uuid.NewString()[:8]}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "push the fix", Source: models.RunSourceObjective,
		Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: actiontest.LoopStoreWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	return &actiontest.LoopStore{T: t, Ctx: ctx, Store: store, Scope: scope, Now: now, Run: run}
}

func requireQueuedAfterDecision(t *testing.T, run *models.AgentRun, decisionID string, at time.Time) {
	t.Helper()
	if run.Status != models.AgentRunStatusQueued || run.WakeCondition != nil || run.LeaseOwner != "" || run.LeaseExpiresAt != nil ||
		!run.AvailableAt.Equal(at) || !run.QueueEnteredAt.Equal(at) || run.LastWakeSignalID != decisionID {
		t.Fatalf("Run after parked decision = %#v", run)
	}
}

func TestLoopParkedApprovalApprovedQueuesRunAndReadiesCall(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		call, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
		f.ParkOn(approval.ID)
		parkedRevision := f.Run.Revision
		at := f.Now.Add(2 * time.Second)
		result, err := f.Decide(f.ApprovalCoordinator(at), approval, models.ApprovalDecisionApprove, "")
		if err != nil {
			t.Fatal(err)
		}
		if !result.Resolved || result.Approval.Status != models.ApprovalStatusApproved || result.Call.Status != models.ActionCallStatusReady || result.Call.ID != call.ID {
			t.Fatalf("resolution = %#v", result)
		}
		run := f.CurrentRun()
		requireQueuedAfterDecision(t, run, "decision-"+approval.ID, at)
		if run.Revision != parkedRevision+1 {
			t.Fatalf("Run revision = %d, want %d", run.Revision, parkedRevision+1)
		}
		claimed, err := store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{Scope: f.Scope, WorkerID: "next-worker", Now: at.Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("the approved Run is not claimable: %#v, %v", claimed, err)
		}
		if replay, err := f.Decide(f.ApprovalCoordinator(at), approval, models.ApprovalDecisionApprove, ""); err != nil || replay.Resolved {
			t.Fatalf("replay = %#v, %v", replay, err)
		}
		if _, err := f.ApprovalCoordinator(at).Resolve(f.Ctx, action.ResolveApprovalRequest{
			Scope: f.Scope, ApprovalID: approval.ID, ExpectedRevision: result.Approval.Revision,
			DecisionID: "other", Decision: models.ApprovalDecisionReject, Principal: actiontest.LoopApprover,
		}); !errors.Is(err, models.ErrApprovalResolved) {
			t.Fatalf("second decision = %v", err)
		}
	})
}

func TestLoopParkedApprovalRejectedQueuesRunAndDeniesCall(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := newLoopApprovalFixture(t, store, &models.BudgetPolicy{MaxActions: 2})
		_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{ReserveBudget: true})
		if len(f.Run.BudgetReservations) != 1 {
			t.Fatalf("reservation = %#v", f.Run.BudgetReservations)
		}
		f.ParkOn(approval.ID)
		at := f.Now.Add(2 * time.Second)
		result, err := f.Decide(f.ApprovalCoordinator(at), approval, models.ApprovalDecisionReject, "not this branch")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusRejected || result.Call.Status != models.ActionCallStatusDenied ||
			result.Call.Error != "rejected: not this branch" || result.Call.CompletedAt == nil {
			t.Fatalf("rejection = %#v", result)
		}
		run := f.CurrentRun()
		requireQueuedAfterDecision(t, run, "decision-"+approval.ID, at)
		if len(run.BudgetReservations) != 0 || run.BudgetUsage.Actions != 0 || len(run.Checkpoint) != 0 {
			t.Fatalf("rejected Run budget/checkpoint = %#v", run)
		}
	})
}

func TestLoopParkedApprovalChangesRequestedRequiresGuidance(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
		f.ParkOn(approval.ID)
		at := f.Now.Add(2 * time.Second)
		if _, err := f.Decide(f.ApprovalCoordinator(at), approval, models.ApprovalDecisionRequestChanges, " "); err == nil || !strings.Contains(err.Error(), "reviewer guidance is required") {
			t.Fatalf("missing guidance = %v", err)
		}
		if run := f.CurrentRun(); run.Status != models.AgentRunStatusWaitingForApproval {
			t.Fatalf("a refused change request moved the Run: %#v", run)
		}
		const guidance = "Push to the staging branch first."
		result, err := f.Decide(f.ApprovalCoordinator(at), approval, models.ApprovalDecisionRequestChanges, guidance)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusChangesRequested || result.Approval.DecisionReason != guidance ||
			result.Call.Status != models.ActionCallStatusDenied || result.Call.Error != "changes_requested: "+guidance ||
			result.Event.EventType != "approval.changes_requested" {
			t.Fatalf("change request = %#v", result)
		}
		requireQueuedAfterDecision(t, f.CurrentRun(), "decision-"+approval.ID, at)
	})
}

func TestLoopParkedApprovalExpiredQueues(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
		f.ParkOn(approval.ID)
		early := f.ApprovalCoordinator(approval.ExpiresAt.Add(-time.Second))
		if _, err := early.ResolveTimeout(f.Ctx, f.Scope, approval.ID, approval.Revision, approval.ID); err == nil || !strings.Contains(err.Error(), "deadline has not elapsed") {
			t.Fatalf("early timeout = %v", err)
		}
		at := approval.ExpiresAt.Add(time.Second)
		result, err := f.ApprovalCoordinator(at).ResolveTimeout(f.Ctx, f.Scope, approval.ID, approval.Revision, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusExpired || result.Call.Status != models.ActionCallStatusDenied || result.Event.EventType != "approval.expired" {
			t.Fatalf("expiry = %#v", result)
		}
		requireQueuedAfterDecision(t, f.CurrentRun(), "approval-timeout:"+approval.ID, at)
	})
}

func TestLoopParkedApprovalTimeoutAutoApproveNeverForAlwaysReview(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, reviewed := f.ProposeLoopApproval("always", actiontest.LoopApprovalOptions{Timeout: models.ApprovalTimeoutApprove, AlwaysReview: true})
		_, plain := f.ProposeLoopApproval("plain", actiontest.LoopApprovalOptions{Timeout: models.ApprovalTimeoutApprove})
		f.ParkOn(reviewed.ID)
		at := reviewed.ExpiresAt.Add(time.Second)
		coordinator := f.ApprovalCoordinator(at)
		result, err := coordinator.ResolveTimeout(f.Ctx, f.Scope, reviewed.ID, reviewed.Revision, reviewed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusExpired || result.Call.Status != models.ActionCallStatusDenied {
			t.Fatalf("an always-review approval auto-approved: %#v", result)
		}
		requireQueuedAfterDecision(t, f.CurrentRun(), "approval-timeout:"+reviewed.ID, at)

		// The other call is still held by the queued Run: its timeout rule applies.
		before := f.CurrentRun()
		auto, err := coordinator.ResolveTimeout(f.Ctx, f.Scope, plain.ID, plain.Revision, plain.ID)
		if err != nil {
			t.Fatal(err)
		}
		if auto.Approval.Status != models.ApprovalStatusApproved || auto.Call.Status != models.ActionCallStatusReady || auto.Event.EventType != "approval.auto_approved" {
			t.Fatalf("timeout auto-approve = %#v", auto)
		}
		if after := f.CurrentRun(); after.Status != models.AgentRunStatusQueued || after.Revision != before.Revision+1 {
			t.Fatalf("held auto-approve Run = %#v (before revision %d)", after, before.Revision)
		}
	})
}

func TestLoopHeldApprovalDecisionBumpsRunRevision(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := newLoopApprovalFixture(t, store, &models.BudgetPolicy{MaxActions: 3})
		_, approved := f.ProposeLoopApproval("approved", actiontest.LoopApprovalOptions{ReserveBudget: true})
		_, rejected := f.ProposeLoopApproval("rejected", actiontest.LoopApprovalOptions{ReserveBudget: true})
		at := f.Now.Add(2 * time.Second)
		coordinator := f.ApprovalCoordinator(at)

		before := f.CurrentRun()
		result, err := f.Decide(coordinator, approved, models.ApprovalDecisionApprove, "")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusApproved || result.Call.Status != models.ActionCallStatusReady {
			t.Fatalf("held approve = %#v", result)
		}
		after := f.CurrentRun()
		if after.Revision != before.Revision+1 || after.Status != models.AgentRunStatusRunning || after.LeaseOwner != actiontest.LoopStoreWorker ||
			after.LeaseExpiresAt == nil || !after.LeaseExpiresAt.Equal(*before.LeaseExpiresAt) || after.WakeCondition != nil ||
			len(after.BudgetReservations) != 2 || after.LastWakeSignalID != before.LastWakeSignalID {
			t.Fatalf("held approve Run = %#v (before %#v)", after, before)
		}

		result, err = f.Decide(coordinator, rejected, models.ApprovalDecisionReject, "no")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusRejected || result.Call.Status != models.ActionCallStatusDenied {
			t.Fatalf("held reject = %#v", result)
		}
		final := f.CurrentRun()
		if final.Revision != after.Revision+1 || final.Status != models.AgentRunStatusRunning || final.LeaseOwner != actiontest.LoopStoreWorker {
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
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
		planned := f.CurrentRun()
		if _, err := f.Decide(f.ApprovalCoordinator(f.Now.Add(time.Second)), approval, models.ApprovalDecisionApprove, ""); err != nil {
			t.Fatal(err)
		}
		parked := models.CloneAgentRun(planned)
		parked.Status = models.AgentRunStatusWaitingForApproval
		parked.WakeCondition = &models.WakeCondition{Type: "approval", Reference: approval.ID}
		parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
		parked.Revision++
		if _, err := store.UpdateAgentRunWithEvent(f.Ctx, parked, planned.Revision, f.Event("stale-park", "run.waiting"), nil); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("stale park = %v", err)
		}
		if run := f.CurrentRun(); run.Status != models.AgentRunStatusRunning {
			t.Fatalf("Run = %#v", run)
		}
	})
}

func TestLoopHeldApprovalApprovalModeSweep(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
		before := f.CurrentRun()
		result, err := f.ApprovalCoordinator(f.Now.Add(time.Second)).ResolveByApprovalMode(f.Ctx, f.Scope, approval.ID, approval.Revision, models.ConversationApprovalSkip, "mode-change")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusApproved || result.Approval.DecisionBy == nil || *result.Approval.DecisionBy != action.ApprovalModePrincipal ||
			result.Call.Status != models.ActionCallStatusReady {
			t.Fatalf("mode sweep = %#v", result)
		}
		if after := f.CurrentRun(); after.Status != models.AgentRunStatusRunning || after.Revision != before.Revision+1 || after.LeaseOwner != actiontest.LoopStoreWorker {
			t.Fatalf("mode sweep Run = %#v", after)
		}
	})
}

func TestLoopSupersededRecordsMessageID(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, held := f.ProposeLoopApproval("held", actiontest.LoopApprovalOptions{})
		_, parked := f.ProposeLoopApproval("parked", actiontest.LoopApprovalOptions{})
		at := f.Now.Add(time.Second)
		coordinator := f.ApprovalCoordinator(at)
		before := f.CurrentRun()
		result, err := coordinator.ResolveSupersededByMessage(f.Ctx, f.Scope, held.ID, held.Revision, "message-1")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusSuperseded || result.Approval.SupersededByMessageID != "message-1" ||
			result.Call.Status != models.ActionCallStatusDenied || result.Call.Error != "superseded: "+action.ApprovalSupersededReason {
			t.Fatalf("held supersede = %#v", result)
		}
		if after := f.CurrentRun(); after.Status != models.AgentRunStatusRunning || after.Revision != before.Revision+1 {
			t.Fatalf("held supersede Run = %#v", after)
		}
		stored, err := store.GetApproval(f.Ctx, f.Scope, held.ID)
		if err != nil || stored.SupersededByMessageID != "message-1" {
			t.Fatalf("stored approval = %#v, %v", stored, err)
		}

		f.ParkOn(parked.ID)
		result, err = coordinator.ResolveSupersededByMessage(f.Ctx, f.Scope, parked.ID, parked.Revision, "message-2")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.SupersededByMessageID != "message-2" || result.Event.EventType != "approval.superseded" || result.Event.CorrelationID != "message-2" {
			t.Fatalf("parked supersede = %#v", result)
		}
		requireQueuedAfterDecision(t, f.CurrentRun(), "superseded:"+parked.ID, at)
	})
}

func TestLoopPausedParkedApprovalResolvesToPausedQueued(t *testing.T) {
	for _, decision := range []models.ApprovalDecision{models.ApprovalDecisionApprove, models.ApprovalDecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
				f := actiontest.NewLoopStore(t, store)
				_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
				f.ParkOn(approval.ID)
				paused, err := agentrun.NewRunCommandService(store).CommandAgentRun(f.Ctx, agentrun.AgentRunCommandRequest{
					Scope: f.Scope, RunID: f.Run.ID, ExpectedRevision: f.Run.Revision, Kind: agentrun.AgentRunCommandPause,
					Actor: models.ActivityActor{Type: "user", ID: "operator"},
				})
				if err != nil || paused.Run.Status != models.AgentRunStatusPaused || paused.Run.PausedFrom != models.AgentRunStatusWaitingForApproval {
					t.Fatalf("pause = %#v, %v", paused, err)
				}
				result, err := f.Decide(f.ApprovalCoordinator(f.Now.Add(time.Second)), approval, decision, "reason")
				if err != nil {
					t.Fatal(err)
				}
				want := models.ActionCallStatusReady
				if decision == models.ApprovalDecisionReject {
					want = models.ActionCallStatusDenied
				}
				if result.Call.Status != want {
					t.Fatalf("call = %#v", result.Call)
				}
				run := f.CurrentRun()
				if run.Status != models.AgentRunStatusPaused || run.PausedFrom != models.AgentRunStatusQueued || run.PausedWakeCondition != nil ||
					run.Revision != paused.Run.Revision+1 {
					t.Fatalf("paused Run = %#v", run)
				}
			})
		})
	}
}

func TestLoopApprovalOfEndedRun(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
		f.SetRunStatus(models.AgentRunStatusFailed)
		ended := f.CurrentRun()
		if _, err := f.Decide(f.ApprovalCoordinator(f.Now.Add(time.Second)), approval, models.ApprovalDecisionApprove, ""); !errors.Is(err, models.ErrInvalidRunTransition) {
			t.Fatalf("decision on an ended Run = %v", err)
		}
		result, err := f.ApprovalCoordinator(approval.ExpiresAt.Add(time.Second)).ResolveTimeout(f.Ctx, f.Scope, approval.ID, approval.Revision, approval.ID)
		if err != nil || result.Approval.Status != models.ApprovalStatusExpired || result.Call.Status != models.ActionCallStatusDenied {
			t.Fatalf("expiry of an ended Run's approval = %#v, %v", result, err)
		}
		if run := f.CurrentRun(); run.Revision != ended.Revision || run.Status != models.AgentRunStatusFailed {
			t.Fatalf("expiry touched the ended Run: %#v", run)
		}
	})
}

// raceRunPortfolio bumps the Run revision with a lease heartbeat right after
// the coordinator reads the Run, the first `races` times.
type raceRunPortfolio struct {
	models.PortfolioStore
	store models.KernelStore
	races atomic.Int32
	now   time.Time
}

func (p *raceRunPortfolio) GetAgentRun(ctx context.Context, scope models.Scope, runID string) (*models.AgentRun, error) {
	run, err := p.PortfolioStore.GetAgentRun(ctx, scope, runID)
	if err == nil && p.races.Add(-1) >= 0 {
		if _, renewErr := p.store.RenewAgentRunLease(ctx, scope, runID, actiontest.LoopStoreWorker, p.now, time.Hour); renewErr != nil {
			return nil, renewErr
		}
	}
	return run, err
}

func TestLoopHeldApprovalRetriesWhenRunMoves(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, approval := f.ProposeLoopApproval("push", actiontest.LoopApprovalOptions{})
		_, starved := f.ProposeLoopApproval("starved", actiontest.LoopApprovalOptions{})
		at := f.Now.Add(time.Second)
		portfolio := &raceRunPortfolio{PortfolioStore: store, store: store, now: at}
		portfolio.races.Store(action.ApprovalResolveAttempts - 1)
		coordinator := action.NewApprovalCoordinator(portfolio, store, action.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return at }))
		result, err := f.Decide(coordinator, approval, models.ApprovalDecisionApprove, "")
		if err != nil || !result.Resolved || result.Call.Status != models.ActionCallStatusReady {
			t.Fatalf("decision after racing heartbeats = %#v, %v", result, err)
		}
		portfolio.races.Store(action.ApprovalResolveAttempts)
		if _, err := f.Decide(coordinator, starved, models.ApprovalDecisionApprove, ""); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("decision losing every race = %v", err)
		}
		if stored, err := store.GetApproval(f.Ctx, f.Scope, starved.ID); err != nil || stored.Status != models.ApprovalStatusPending {
			t.Fatalf("starved approval = %#v, %v", stored, err)
		}
	})
}
