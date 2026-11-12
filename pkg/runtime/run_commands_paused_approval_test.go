package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Paused approvals on the run loop (12-tests §5.2,
// TestLoopPausedApprovalCancelAndExpiry): an operator pauses a Run parked
// on an approval; canceling closes the approval, its call and the tool entry
// together, and an expiry while paused is the tool result once resumed.

func (f *loopFixture) commandRun(runID string, kind AgentRunCommandKind) *AgentRun {
	f.t.Helper()
	current := f.mustRun(runID)
	commands := NewRunCommandService(f.chats, models.WithClock(f.clock.Now))
	result, err := commands.CommandAgentRun(f.ctx, AgentRunCommandRequest{
		Scope: f.scope, RunID: runID, ExpectedRevision: current.Revision, Kind: kind,
		Actor: ActivityActor{Type: "user", ID: loopFixtureUser},
	})
	if err != nil {
		f.t.Fatalf("%s run: %v", kind, err)
	}
	return result.Run
}

// expireApproval lets the Run's single approval run out and resolves it the
// way the approval sweep does.
func (f *loopFixture) expireApproval(runID string) *ApprovalResolutionResult {
	f.t.Helper()
	approvals := f.approvals(runID)
	if len(approvals) != 1 {
		f.t.Fatalf("approvals = %d, want 1", len(approvals))
	}
	approval := approvals[0]
	f.clock.Advance(approval.ExpiresAt.Sub(f.clock.Now()) + time.Second)
	result, err := f.approve.ResolveTimeout(f.ctx, f.scope, approval.ID, approval.Revision, approval.ID)
	if err != nil {
		f.t.Fatalf("expire approval: %v", err)
	}
	return result
}

func TestLoopPausedApprovalCancelAndExpiry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "cancel_pending"
		if expired {
			name = "cancel_expired"
		}
		t.Run(name, func(t *testing.T) {
			eachStore(t, "loop_paused_approval_"+name, func(t *testing.T, store KernelStore) {
				f := newApprovalScenario(t, store)
				_, run := f.parkedApproval()
				approval, call := assertParkedOnApproval(t, f, run)
				paused := f.commandRun(run.ID, AgentRunCommandPause)
				if paused.Status != AgentRunStatusPaused || paused.PausedFrom != AgentRunStatusWaitingForApproval || paused.PausedWakeCondition == nil {
					t.Fatalf("paused = %#v", paused)
				}
				if expired {
					result := f.expireApproval(run.ID)
					if result.Run == nil || result.Run.Status != AgentRunStatusPaused || result.Run.PausedFrom != AgentRunStatusQueued || result.Run.PausedWakeCondition != nil {
						t.Fatalf("expiry while paused = %#v", result.Run)
					}
				}
				canceled := f.commandRun(run.ID, AgentRunCommandCancel)
				if canceled.Status != AgentRunStatusCanceled || canceled.PausedWakeCondition != nil || canceled.PausedFrom != "" || len(canceled.BudgetReservations) != 0 {
					t.Fatalf("canceled = %#v", canceled)
				}
				gotApproval, err := f.raw.GetApproval(f.ctx, f.scope, approval.ID)
				if err != nil {
					t.Fatal(err)
				}
				gotCall, err := f.raw.GetActionCall(f.ctx, f.scope, call.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !expired && (gotApproval.Status != ApprovalStatusCanceled || gotCall.Status != ActionCallStatusCanceled || gotCall.CompletedAt == nil) {
					t.Fatalf("pending approval/call did not close together: %s / %s", gotApproval.Status, gotCall.Status)
				}
				if expired && (gotApproval.Status != ApprovalStatusExpired || gotCall.Status != ActionCallStatusDenied) {
					t.Fatalf("cancel rewrote the expired approval: %s / %s", gotApproval.Status, gotCall.Status)
				}
				// The tool entry is closed: no later Run of the chat sees an
				// unanswered call.
				tools := f.toolEntries(run.ID)
				if len(tools) != 1 || !tools[0].State.Terminal() || tools[0].State == ToolEntryCompleted {
					t.Fatalf("tool entry after cancel = %v", f.entryTypes(run.ID))
				}
				if _, err := f.approve.Resolve(f.ctx, ResolveApprovalRequest{
					Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: gotApproval.Revision, DecisionID: "late-approval",
					Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: loopFixtureUser},
				}); !errors.Is(err, ErrApprovalResolved) {
					t.Fatalf("closed approval accepted a late decision: %v", err)
				}
				assertActionLifecycleUnclaimable(t, f.raw, gotCall, f.clock.Now().Add(2*time.Hour))
				if busy, err := f.raw.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(f.ctx, SkillRuntimeUsageFilter{
					Scope: f.scope, DeploymentID: gotCall.DeploymentID, BindingID: gotCall.BindingID, SkillID: gotCall.SkillID, SkillVersion: gotCall.SkillVersion,
				}); err != nil || busy {
					t.Fatalf("closed approval kept runtime usage: %v %v", busy, err)
				}
				if f.dispatch.count("") != 0 {
					t.Fatal("a canceled call was dispatched")
				}
			})
		})
	}

	t.Run("expiry_resumes_with_outcome", func(t *testing.T) {
		eachStore(t, "loop_paused_approval_expiry", func(t *testing.T, store KernelStore) {
			f := newApprovalScenario(t, store)
			conversation, run := f.parkedApproval()
			f.commandRun(run.ID, AgentRunCommandPause)
			result := f.expireApproval(run.ID)
			if result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied ||
				result.Run.Status != AgentRunStatusPaused || result.Run.PausedFrom != AgentRunStatusQueued ||
				result.Run.PausedWakeCondition != nil || result.Run.WakeCondition != nil || len(result.Run.BudgetReservations) != 0 {
				t.Fatalf("expiry did not close the paused wait: %#v", result)
			}
			// Paused: nothing claims it.
			if claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{Scope: f.scope, WorkerID: f.workerID, Now: f.clock.Now(), LeaseDuration: loopFixtureLease, AgingInterval: time.Minute}); err != nil || claimed != nil {
				t.Fatalf("claimed a paused Run: %#v, %v", claimed, err)
			}
			resumed := f.commandRun(run.ID, AgentRunCommandResume)
			if resumed.Status != AgentRunStatusQueued || resumed.WakeCondition != nil || resumed.PausedWakeCondition != nil {
				t.Fatalf("resume restored the expired wait: %#v", resumed)
			}
			done := f.driveToRest(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			tool := f.toolEntries(run.ID)[0]
			if tool.State != ToolEntryError || !strings.Contains(toolEntryText(tool), string(ToolResultExpired)) {
				t.Fatalf("tool entry = %s %q", tool.State, toolEntryText(tool))
			}
			if replies := f.agentReplies(conversation.ID); len(replies) != 1 || !strings.HasPrefix(replies[0], "Not closed: ") {
				t.Fatalf("replies = %q", replies)
			}
			if f.dispatch.count("update_status") != 0 {
				t.Fatal("an expired call was dispatched")
			}
			assertActionLifecycleUnclaimable(t, f.raw, result.Call, f.clock.Now().Add(2*time.Hour))
		})
	})
}
