package runtime

import (
	"errors"
	"testing"
	"time"
)

func TestRunCommandsCancelPausedApprovalAtomically(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "pending"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
				proposal := createApprovalForStore(t, store, now)
				commands := NewRunCommandService(store)
				commands.now = func() time.Time { return now.Add(2 * time.Second) }
				paused, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{
					Scope: proposal.Run.Scope, RunID: proposal.Run.ID, ExpectedRevision: proposal.Run.Revision,
					Kind: AgentRunCommandPause, Actor: ActivityActor{Type: "user", ID: "operator"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if expired {
					coordinator := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
					coordinator.now = func() time.Time { return proposal.Approval.ExpiresAt.Add(time.Second) }
					result, err := coordinator.ResolveTimeout(t.Context(), proposal.Approval.Scope, proposal.Approval.ID, proposal.Approval.Revision, "expire-paused-approval")
					if err != nil {
						t.Fatal(err)
					}
					paused.Run = result.Run
				}
				canceled, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{
					Scope: paused.Run.Scope, RunID: paused.Run.ID, ExpectedRevision: paused.Run.Revision,
					Kind: AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "operator"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if canceled.Run.Status != AgentRunStatusCanceled || canceled.Run.PausedWakeCondition != nil || canceled.Run.PausedFrom != "" || len(canceled.Run.BudgetReservations) != 0 {
					t.Fatalf("paused approval cancellation left a continuation or reservation: %#v", canceled.Run)
				}
				approval, err := store.GetApproval(t.Context(), proposal.Approval.Scope, proposal.Approval.ID)
				if err != nil {
					t.Fatal(err)
				}
				call, err := store.GetActionCall(t.Context(), proposal.Call.Scope, proposal.Call.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !expired && (approval.Status != ApprovalStatusCanceled || call.Status != ActionCallStatusCanceled || call.CompletedAt == nil) {
					t.Fatalf("pending approval/call did not close atomically: approval=%#v call=%#v", approval, call)
				}
				if expired && (approval.Status != ApprovalStatusExpired || call.Status != ActionCallStatusDenied) {
					t.Fatalf("cancellation rewrote expired approval history: approval=%#v call=%#v", approval, call)
				}
				coordinator := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
				coordinator.now = func() time.Time { return now.Add(3 * time.Second) }
				if _, err := coordinator.Resolve(t.Context(), ResolveApprovalRequest{Scope: approval.Scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision, DecisionID: "late-approval", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: "alice"}}); !errors.Is(err, ErrApprovalResolved) {
					t.Fatalf("canceled approval accepted a late decision: %v", err)
				}
				assertActionLifecycleUnclaimable(t, store, call, now.Add(2*time.Hour))
				if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), SkillRuntimeUsageFilter{Scope: call.Scope, DeploymentID: call.DeploymentID, BindingID: call.BindingID, SkillID: call.SkillID, SkillVersion: call.SkillVersion}); err != nil || busy {
					t.Fatalf("terminal approval retained runtime usage: %v %v", busy, err)
				}
			})
		})
	}
}

func TestPausedApprovalExpiryResumesWithTerminalOutcome(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
		proposal := createApprovalForStoreWithTimeout(t, store, now, ApprovalTimeoutApprove)
		commands := NewRunCommandService(store)
		commands.now = func() time.Time { return now.Add(2 * time.Second) }
		paused, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: proposal.Run.Scope, RunID: proposal.Run.ID, ExpectedRevision: proposal.Run.Revision, Kind: AgentRunCommandPause, Actor: ActivityActor{Type: "user", ID: "operator"}})
		if err != nil {
			t.Fatal(err)
		}
		coordinator := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
		coordinator.now = func() time.Time { return proposal.Approval.ExpiresAt.Add(time.Second) }
		result, err := coordinator.ResolveTimeout(t.Context(), proposal.Approval.Scope, proposal.Approval.ID, proposal.Approval.Revision, "paused-expiry")
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied || result.Run.Status != AgentRunStatusPaused || result.Run.PausedFrom != AgentRunStatusQueued || result.Run.PausedWakeCondition != nil || result.Run.WakeCondition != nil {
			t.Fatalf("expiry did not close the paused dependency: %#v", result)
		}
		if paused.Run.Checkpoint["lastAction"] != nil {
			t.Fatal("fixture already contained a terminal action")
		}
		last, _ := result.Run.Checkpoint["lastAction"].(map[string]interface{})
		if last["actionCallId"] != proposal.Call.ID || last["status"] != string(ActionCallStatusDenied) || len(result.Run.BudgetReservations) != 0 {
			t.Fatalf("paused expiry did not retain the denied result: %#v", result.Run)
		}
		resumed, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: result.Run.Scope, RunID: result.Run.ID, ExpectedRevision: result.Run.Revision, Kind: AgentRunCommandResume, Actor: ActivityActor{Type: "user", ID: "operator"}})
		if err != nil || resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil || resumed.Run.PausedWakeCondition != nil {
			t.Fatalf("resume restored an expired approval dependency: %#v, %v", resumed, err)
		}
		assertActionLifecycleUnclaimable(t, store, result.Call, now.Add(2*time.Hour))
	})
}
