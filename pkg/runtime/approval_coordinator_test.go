package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApprovalCoordinatorAtomicallyResolvesAndWakesAcrossStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "approval.db"))
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
			approvalCoordinator := NewApprovalCoordinator(store, store, ApprovalAuthorizerFunc(func(_ context.Context, principal ApprovalPrincipal, approval *ApprovalCheckpoint) error {
				if principal.ID != "alice" || approval.ID != proposal.Approval.ID {
					return errors.New("not authorized")
				}
				return nil
			}))
			approvalCoordinator.now = func() time.Time { return now.Add(2 * time.Second) }
			approvalCoordinator.newID = func() string { return "approval-event" }
			result, err := approvalCoordinator.Resolve(ctx, ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "decision-1", Approve: true, Principal: ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "change reviewed",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Resolved || result.Approval.Status != ApprovalStatusApproved || result.Call.Status != ActionCallStatusReady || result.Run.Status != AgentRunStatusWaitingForDependency || result.Run.WakeCondition == nil || result.Run.WakeCondition.Reference != result.Call.ID {
				t.Fatalf("resolution mismatch: %#v", result)
			}
			retry, err := approvalCoordinator.Resolve(ctx, ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "decision-1", Approve: true, Principal: ApprovalPrincipal{Type: "user", ID: "alice"},
			})
			if err != nil || retry.Resolved {
				t.Fatalf("idempotent retry = %#v, %v", retry, err)
			}
			_, err = approvalCoordinator.Resolve(ctx, ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: result.Approval.Revision,
				DecisionID: "decision-2", Approve: false, Principal: ApprovalPrincipal{Type: "user", ID: "alice"},
			})
			if !errors.Is(err, ErrApprovalResolved) {
				t.Fatalf("second decision error = %v", err)
			}
			events, err := store.ListActivity(ctx, ActivityFilter{Scope: proposal.Approval.Scope, RunID: proposal.Approval.RunID})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[1].EventType != "approval.approved" {
				t.Fatalf("approval activity mismatch: %#v", events)
			}
			claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: proposal.Approval.Scope, WorkerID: "next-worker", Now: now.Add(3 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed != nil {
				t.Fatalf("run resumed before its approved action completed: %#v, %v", claimed, err)
			}
		})
	}
}

func TestApprovalCoordinatorFailsClosedAndPersistsExpiry(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	proposal := createApprovalForStore(t, store, now)
	coordinator := NewApprovalCoordinator(store, store, ApprovalAuthorizerFunc(func(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error {
		return errors.New("RBAC denied")
	}))
	coordinator.now = func() time.Time { return now.Add(2 * time.Second) }
	_, err := coordinator.Resolve(ctx, ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "unauthorized", Approve: true, Principal: ApprovalPrincipal{Type: "user", ID: "mallory"},
	})
	if err == nil || !strings.Contains(err.Error(), "RBAC denied") {
		t.Fatalf("authorization error = %v", err)
	}
	unchanged, err := store.GetApproval(ctx, proposal.Approval.Scope, proposal.Approval.ID)
	if err != nil || unchanged.Status != ApprovalStatusPending {
		t.Fatalf("unauthorized decision mutated approval: %#v, %v", unchanged, err)
	}
	coordinator.now = func() time.Time { return proposal.Approval.ExpiresAt.Add(time.Second) }
	expired, err := coordinator.Resolve(ctx, ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "expiry", Approve: true, Principal: ApprovalPrincipal{Type: "system", ID: "expiry-worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.Approval.Status != ApprovalStatusExpired || expired.Call.Status != ActionCallStatusDenied || expired.Run.Status != AgentRunStatusQueued ||
		expired.Run.BudgetUsage.Actions != 0 || len(expired.Run.BudgetReservations) != 0 || expired.Run.BudgetState != BudgetStateActive {
		t.Fatalf("expiry resolution mismatch: %#v", expired)
	}
}

func TestApprovalCoordinatorExpiresApprovalAfterRunLeavesWaitingState(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	proposal := createApprovalForStore(t, store, now)
	paused, err := NewRunCommandService(store).CommandAgentRun(t.Context(), AgentRunCommandRequest{
		Scope: proposal.Run.Scope, RunID: proposal.Run.ID, ExpectedRevision: proposal.Run.Revision, Kind: AgentRunCommandPause,
		Actor: ActivityActor{Type: "user", ID: "operator"},
	})
	if err != nil || paused.Run.Status != AgentRunStatusPaused {
		t.Fatalf("pause pending approval run = %#v, %v", paused, err)
	}
	coordinator := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
	coordinator.now = func() time.Time { return proposal.Approval.ExpiresAt.Add(time.Second) }
	expired, err := coordinator.Resolve(t.Context(), ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "orphan-expiry", Principal: ApprovalPrincipal{Type: "system", ID: "expiry-worker"}, Reason: "deadline elapsed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.Approval.Status != ApprovalStatusExpired || expired.Call.Status != ActionCallStatusDenied ||
		expired.Run.Status != AgentRunStatusPaused || len(expired.Run.BudgetReservations) != 0 {
		t.Fatalf("orphan expiry = %#v", expired)
	}
}

func TestApprovalCoordinatorPersistsRejectedActionOutcome(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "approval-rejection.db"))
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
			coordinator := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
			coordinator.now = func() time.Time { return now.Add(2 * time.Second) }
			result, err := coordinator.Resolve(t.Context(), ResolveApprovalRequest{
				Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "reject-release", Approve: false, Principal: ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "change is not authorized",
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Approval.Status != ApprovalStatusRejected || result.Call.Status != ActionCallStatusDenied || result.Call.CompletedAt == nil || result.Run.Status != AgentRunStatusQueued {
				t.Fatalf("rejection result = %#v", result)
			}
			last, _ := result.Run.Checkpoint["lastAction"].(map[string]interface{})
			if fmt.Sprint(last["status"]) != string(ActionCallStatusDenied) || fmt.Sprint(last["approvalId"]) != proposal.Approval.ID || fmt.Sprint(last["approvalStatus"]) != string(ApprovalStatusRejected) ||
				fmt.Sprint(last["error"]) != "rejected: change is not authorized" || len(actionHistoryEntries(result.Run.Checkpoint)) != 1 {
				t.Fatalf("rejection checkpoint = %#v", result.Run.Checkpoint)
			}
		})
	}
}

func createApprovalForStore(t *testing.T, store KernelStore, now time.Time) *ActionProposalResult {
	t.Helper()
	ctx := context.Background()
	catalog, scope := governedActionCatalog(t)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective,
		Budget: &BudgetPolicy{MaxActions: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "alice"}}, ApprovalTTL: time.Hour}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	ids := []string{"call", "approval", "proposal-event"}
	coordinator.newID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: claimed.ID, WorkerID: "worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}
