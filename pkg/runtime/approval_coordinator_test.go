package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestApprovalCoordinatorAtomicallyResolvesAndWakesAcrossStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "approval.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			ctx := context.Background()
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			proposal := createApprovalForStore(t, store, now)
			approvalCoordinator := runtime.NewApprovalCoordinator(store, store, runtime.ApprovalAuthorizerFunc(func(_ context.Context, principal models.ApprovalPrincipal, approval *models.ApprovalCheckpoint) error {
				if principal.ID != "alice" || approval.ID != proposal.Approval.ID {
					return errors.New("not authorized")
				}
				return nil
			}), models.WithClock(func() time.Time { return now.Add(2 * time.Second) }))
			approvalCoordinator.SetNewID(func() string { return "approval-event" })
			result, err := approvalCoordinator.Resolve(ctx, runtime.ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "decision-1", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "change reviewed",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Resolved || result.Approval.Status != models.ApprovalStatusApproved || result.Call.Status != models.ActionCallStatusReady || result.Run.Status != models.AgentRunStatusQueued || result.Run.WakeCondition != nil || result.Run.LastWakeSignalID != "decision-1" {
				t.Fatalf("resolution mismatch: %#v", result)
			}
			retry, err := approvalCoordinator.Resolve(ctx, runtime.ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "decision-1", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "alice"},
			})
			if err != nil || retry.Resolved {
				t.Fatalf("idempotent retry = %#v, %v", retry, err)
			}
			_, err = approvalCoordinator.Resolve(ctx, runtime.ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: result.Approval.Revision,
				DecisionID: "decision-2", Decision: models.ApprovalDecisionReject, Principal: models.ApprovalPrincipal{Type: "user", ID: "alice"},
			})
			if !errors.Is(err, models.ErrApprovalResolved) {
				t.Fatalf("second decision error = %v", err)
			}
			events, err := store.ListActivity(ctx, models.ActivityFilter{Scope: proposal.Approval.Scope, RunID: proposal.Approval.RunID})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 3 || events[2].EventType != "approval.approved" {
				t.Fatalf("approval activity mismatch: %#v", events)
			}
			// The approved call runs inline when the loop resumes the Run.
			claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: proposal.Approval.Scope, WorkerID: "next-worker", Now: now.Add(3 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil || claimed.ID != proposal.Run.ID {
				t.Fatalf("approved Run claim = %#v, %v", claimed, err)
			}
		})
	}
}

func TestApprovalCoordinatorFailsClosedAndPersistsExpiry(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	proposal := createApprovalForStore(t, store, now)
	coordinatorClock := func() time.Time { return now.Add(2 * time.Second) }
	coordinator := runtime.NewApprovalCoordinator(store, store, runtime.ApprovalAuthorizerFunc(func(context.Context, models.ApprovalPrincipal, *models.ApprovalCheckpoint) error {
		return errors.New("RBAC denied")
	}), models.WithClock(func() time.Time { return coordinatorClock() }))
	_, err := coordinator.Resolve(ctx, runtime.ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "unauthorized", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "mallory"},
	})
	if err == nil || !strings.Contains(err.Error(), "RBAC denied") {
		t.Fatalf("authorization error = %v", err)
	}
	unchanged, err := store.GetApproval(ctx, proposal.Approval.Scope, proposal.Approval.ID)
	if err != nil || unchanged.Status != models.ApprovalStatusPending {
		t.Fatalf("unauthorized decision mutated approval: %#v, %v", unchanged, err)
	}
	coordinatorClock = func() time.Time { return proposal.Approval.ExpiresAt.Add(time.Second) }
	expired, err := coordinator.Resolve(ctx, runtime.ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "expiry", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "system", ID: "expiry-worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.Approval.Status != models.ApprovalStatusExpired || expired.Call.Status != models.ActionCallStatusDenied || expired.Run.Status != models.AgentRunStatusQueued ||
		expired.Run.BudgetUsage.Actions != 0 || len(expired.Run.BudgetReservations) != 0 || expired.Run.BudgetState != models.BudgetStateActive {
		t.Fatalf("expiry resolution mismatch: %#v", expired)
	}
}

func TestApprovalCoordinatorAppliesReviewedTimeoutApprovalAcrossStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "approval-timeout.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			proposal := createApprovalForStoreWithTimeout(t, store, now, models.ApprovalTimeoutApprove)
			stored, err := store.GetApproval(t.Context(), proposal.Approval.Scope, proposal.Approval.ID)
			if err != nil || stored.TimeoutDecision != models.ApprovalTimeoutApprove {
				t.Fatalf("persisted timeout policy = %#v, %v", stored, err)
			}

			coordinatorClock := func() time.Time { return stored.ExpiresAt.Add(-time.Second) }
			coordinator := runtime.NewApprovalCoordinator(store, store, runtime.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return coordinatorClock() }))
			if _, err = coordinator.ResolveTimeout(t.Context(), stored.Scope, stored.ID, stored.Revision, stored.ID); err == nil || !strings.Contains(err.Error(), "deadline has not elapsed") {
				t.Fatalf("early timeout resolution = %v", err)
			}
			coordinatorClock = func() time.Time { return stored.ExpiresAt.Add(time.Second) }
			result, err := coordinator.ResolveTimeout(t.Context(), stored.Scope, stored.ID, stored.Revision, stored.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Approval.Status != models.ApprovalStatusApproved || result.Call.Status != models.ActionCallStatusReady ||
				result.Run.Status != models.AgentRunStatusQueued || result.Run.WakeCondition != nil {
				t.Fatalf("timeout approval = %#v", result)
			}
			if result.Event == nil || result.Event.EventType != "approval.auto_approved" || result.Event.Actor.ID != "approval-timeout-worker" {
				t.Fatalf("timeout audit = %#v", result.Event)
			}
			retry, err := coordinator.ResolveTimeout(t.Context(), stored.Scope, stored.ID, stored.Revision, stored.ID)
			if err != nil || retry.Resolved {
				t.Fatalf("timeout retry = %#v, %v", retry, err)
			}
		})
	}
}

func TestApprovalCoordinatorExpiresApprovalAfterRunLeavesWaitingState(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	proposal := createApprovalForStore(t, store, now)
	paused, err := agentrun.NewRunCommandService(store).CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{
		Scope: proposal.Run.Scope, RunID: proposal.Run.ID, ExpectedRevision: proposal.Run.Revision, Kind: agentrun.AgentRunCommandPause,
		Actor: models.ActivityActor{Type: "user", ID: "operator"},
	})
	if err != nil || paused.Run.Status != models.AgentRunStatusPaused {
		t.Fatalf("pause pending approval run = %#v, %v", paused, err)
	}
	coordinator := runtime.NewApprovalCoordinator(store, store, runtime.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return proposal.Approval.ExpiresAt.Add(time.Second) }))
	expired, err := coordinator.Resolve(t.Context(), runtime.ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "orphan-expiry", Principal: models.ApprovalPrincipal{Type: "system", ID: "expiry-worker"}, Reason: "deadline elapsed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.Approval.Status != models.ApprovalStatusExpired || expired.Call.Status != models.ActionCallStatusDenied ||
		expired.Run.Status != models.AgentRunStatusPaused || expired.Run.PausedFrom != models.AgentRunStatusQueued || expired.Run.PausedWakeCondition != nil ||
		len(expired.Run.BudgetReservations) != 0 {
		t.Fatalf("orphan expiry = %#v", expired)
	}
}

func TestApprovalCoordinatorPersistsRejectedActionOutcome(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "approval-rejection.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			proposal := createApprovalForStore(t, store, now)
			coordinator := runtime.NewApprovalCoordinator(store, store, runtime.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return now.Add(2 * time.Second) }))
			result, err := coordinator.Resolve(t.Context(), runtime.ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "reject-release", Decision: models.ApprovalDecisionReject, Principal: models.ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "change is not authorized",
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Approval.Status != models.ApprovalStatusRejected || result.Call.Status != models.ActionCallStatusDenied || result.Call.CompletedAt == nil ||
				result.Call.Error != "rejected: change is not authorized" || result.Run.Status != models.AgentRunStatusQueued {
				t.Fatalf("rejection result = %#v", result)
			}
			// The outcome lives on the call; the Run keeps no action checkpoint.
			stored, err := store.GetActionCall(t.Context(), proposal.Call.Scope, proposal.Call.ID)
			if err != nil || stored.Status != models.ActionCallStatusDenied || stored.Error != result.Call.Error || len(result.Run.Checkpoint) != 0 || len(result.Run.BudgetReservations) != 0 {
				t.Fatalf("persisted rejection = %#v, run checkpoint %#v, %v", stored, result.Run.Checkpoint, err)
			}
		})
	}
}

func TestApprovalCoordinatorResumesRunWithReviewerGuidance(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	proposal := createApprovalForStore(t, store, now)
	coordinator := runtime.NewApprovalCoordinator(store, store, runtime.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return now.Add(time.Second) }))

	_, err := coordinator.Resolve(t.Context(), runtime.ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID,
		ExpectedRevision: proposal.Approval.Revision, DecisionID: "missing-guidance",
		Decision: models.ApprovalDecisionRequestChanges, Principal: models.ApprovalPrincipal{Type: "user", ID: "alice"},
	})
	if err == nil || !strings.Contains(err.Error(), "reviewer guidance is required") {
		t.Fatalf("missing guidance error = %v", err)
	}

	const guidance = "Keep the draft, but use r/vibecoding instead."
	result, err := coordinator.Resolve(t.Context(), runtime.ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID,
		ExpectedRevision: proposal.Approval.Revision, DecisionID: "request-revision",
		Decision: models.ApprovalDecisionRequestChanges, Principal: models.ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: guidance,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval.Status != models.ApprovalStatusChangesRequested || result.Approval.DecisionReason != guidance ||
		result.Call.Status != models.ActionCallStatusDenied || result.Run.Status != models.AgentRunStatusQueued || result.Run.WakeCondition != nil {
		t.Fatalf("change request result = %#v", result)
	}
	if result.Event == nil || result.Event.EventType != "approval.changes_requested" || fmt.Sprint(result.Event.Payload["decisionReason"]) != guidance {
		t.Fatalf("change request activity = %#v", result.Event)
	}
	replay, err := coordinator.Resolve(t.Context(), runtime.ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID,
		ExpectedRevision: proposal.Approval.Revision, DecisionID: "request-revision",
		Decision: models.ApprovalDecisionRequestChanges, Principal: models.ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: guidance,
	})
	if err != nil || replay.Resolved {
		t.Fatalf("change request replay = %#v, %v", replay, err)
	}
}

func createApprovalForStore(t *testing.T, store models.KernelStore, now time.Time) *models.ActionProposalResult {
	return createApprovalForStoreWithTimeout(t, store, now, "")
}

// createApprovalForStoreWithTimeout commits a loop call of step-1 that waits
// on its approval and reserves one action of the Run budget, then parks the
// Run on that approval as the loop does at step end. The result's Run is the
// parked Run.
func createApprovalForStoreWithTimeout(t *testing.T, store models.KernelStore, now time.Time, timeout models.ApprovalTimeoutDecision) *models.ActionProposalResult {
	t.Helper()
	ctx := context.Background()
	_, scope := governedActionCatalog(t)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: models.RunSourceObjective,
		Budget: &models.BudgetPolicy{MaxActions: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	proposedAt := now.Add(time.Second)
	call := &models.ActionCall{
		ID: "call", Scope: scope, RunID: claimed.ID, TurnID: "step-1", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		Status: models.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction, SideEffect: skill.SideEffectExternal,
		Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "tool-call:deploy", ApprovalID: "approval",
		MaxAttempts: 3, AvailableAt: proposedAt, Revision: 1, CreatedAt: proposedAt, UpdatedAt: proposedAt,
	}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	approval := &models.ApprovalCheckpoint{
		ID: "approval", Scope: scope, RunID: claimed.ID, ActionCallID: call.ID, Status: models.ApprovalStatusPending,
		Risk: skill.RiskLevelProduction, Summary: "Deploy the release to production",
		ProposedAction:    map[string]interface{}{"skillId": "release", "action": "deploy", "arguments": map[string]interface{}{"environment": "production"}},
		EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "alice"}}, TimeoutDecision: timeout,
		ExpiresAt: proposedAt.Add(time.Hour), Revision: 1, CreatedAt: proposedAt, UpdatedAt: proposedAt,
	}
	reserved := models.CloneAgentRun(claimed)
	if err := models.ReserveRunBudget(reserved, models.BudgetReservation{ID: agentrun.ActionBudgetReservationID(call.ID), Usage: models.BudgetUsage{Actions: 1}, CreatedAt: proposedAt}); err != nil {
		t.Fatal(err)
	}
	reserved.Revision++
	reserved.UpdatedAt = proposedAt
	event := func(id, eventType string) *models.ActivityEvent {
		return &models.ActivityEvent{ID: id, Scope: scope, EventType: eventType, Severity: models.ActivitySeverityInfo, AgentID: "release-agent", RunID: claimed.ID,
			Actor: models.ActivityActor{Type: "worker", ID: "worker"}, Summary: approval.Summary, Visibility: models.ActivityVisibilityScope, CreatedAt: proposedAt}
	}
	proposal, err := store.CreateActionProposal(ctx, models.ActionProposalRecord{
		Call: call, Approval: approval, Run: reserved, ExpectedRunRevision: claimed.Revision,
		Lease: &models.AgentRunLeaseGuard{WorkerID: "worker", Now: proposedAt}, Event: event("proposal-event", "action.approval_requested"),
	})
	if err != nil {
		t.Fatal(err)
	}
	parked := models.CloneAgentRun(proposal.Run)
	parked.Status = models.AgentRunStatusWaitingForApproval
	parked.WakeCondition = &models.WakeCondition{Type: "approval", Reference: approval.ID}
	parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
	parked.Revision++
	parked.UpdatedAt = proposedAt
	if _, err := store.UpdateAgentRunWithEvent(ctx, parked, proposal.Run.Revision, event("park-event", "run.waiting"), nil); err != nil {
		t.Fatal(err)
	}
	if proposal.Run, err = store.GetAgentRun(ctx, scope, claimed.ID); err != nil {
		t.Fatal(err)
	}
	return proposal
}
