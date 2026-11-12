package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
)

// Paused approvals on the run loop (12-tests §5.2,
// TestLoopPausedApprovalCancelAndExpiry): an operator pauses a Run parked
// on an approval; canceling closes the approval, its call and the tool entry
// together, and an expiry while paused is the tool result once resumed.

func TestLoopPausedApprovalCancelAndExpiry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "cancel_pending"
		if expired {
			name = "cancel_expired"
		}
		t.Run(name, func(t *testing.T) {
			looptest.EachStore(t, "loop_paused_approval_"+name, func(t *testing.T, store KernelStore) {
				f := newApprovalScenario(t, store)
				_, run := f.ParkedApproval()
				approval, call := assertParkedOnApproval(t, f, run)
				paused := f.CommandRun(run.ID, agentrun.AgentRunCommandPause)
				if paused.Status != AgentRunStatusPaused || paused.PausedFrom != AgentRunStatusWaitingForApproval || paused.PausedWakeCondition == nil {
					t.Fatalf("paused = %#v", paused)
				}
				if expired {
					result := f.ExpireApproval(run.ID)
					if result.Run == nil || result.Run.Status != AgentRunStatusPaused || result.Run.PausedFrom != AgentRunStatusQueued || result.Run.PausedWakeCondition != nil {
						t.Fatalf("expiry while paused = %#v", result.Run)
					}
				}
				canceled := f.CommandRun(run.ID, agentrun.AgentRunCommandCancel)
				if canceled.Status != AgentRunStatusCanceled || canceled.PausedWakeCondition != nil || canceled.PausedFrom != "" || len(canceled.BudgetReservations) != 0 {
					t.Fatalf("canceled = %#v", canceled)
				}
				gotApproval, err := f.Raw.GetApproval(f.Ctx, f.Scope, approval.ID)
				if err != nil {
					t.Fatal(err)
				}
				gotCall, err := f.Raw.GetActionCall(f.Ctx, f.Scope, call.ID)
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
				tools := f.ToolEntries(run.ID)
				if len(tools) != 1 || !tools[0].State.Terminal() || tools[0].State == ToolEntryCompleted {
					t.Fatalf("tool entry after cancel = %v", f.EntryTypes(run.ID))
				}
				if _, err := f.Approve.Resolve(f.Ctx, action.ResolveApprovalRequest{
					Scope: f.Scope, ApprovalID: approval.ID, ExpectedRevision: gotApproval.Revision, DecisionID: "late-approval",
					Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: looptest.FixtureUser},
				}); !errors.Is(err, ErrApprovalResolved) {
					t.Fatalf("closed approval accepted a late decision: %v", err)
				}
				actiontest.AssertUnclaimable(t, f.Raw, gotCall, f.Clock.Now().Add(2*time.Hour))
				if busy, err := f.Raw.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(f.Ctx, SkillRuntimeUsageFilter{
					Scope: f.Scope, DeploymentID: gotCall.DeploymentID, BindingID: gotCall.BindingID, SkillID: gotCall.SkillID, SkillVersion: gotCall.SkillVersion,
				}); err != nil || busy {
					t.Fatalf("closed approval kept runtime usage: %v %v", busy, err)
				}
				if f.Dispatch.Count("") != 0 {
					t.Fatal("a canceled call was dispatched")
				}
			})
		})
	}

	t.Run("expiry_resumes_with_outcome", func(t *testing.T) {
		looptest.EachStore(t, "loop_paused_approval_expiry", func(t *testing.T, store KernelStore) {
			f := newApprovalScenario(t, store)
			conversation, run := f.ParkedApproval()
			f.CommandRun(run.ID, agentrun.AgentRunCommandPause)
			result := f.ExpireApproval(run.ID)
			if result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied ||
				result.Run.Status != AgentRunStatusPaused || result.Run.PausedFrom != AgentRunStatusQueued ||
				result.Run.PausedWakeCondition != nil || result.Run.WakeCondition != nil || len(result.Run.BudgetReservations) != 0 {
				t.Fatalf("expiry did not close the paused wait: %#v", result)
			}
			// Paused: nothing claims it.
			if claimed, err := f.Store.ClaimNextAgentRun(f.Ctx, AgentRunClaim{Scope: f.Scope, WorkerID: f.WorkerID, Now: f.Clock.Now(), LeaseDuration: looptest.FixtureLease, AgingInterval: time.Minute}); err != nil || claimed != nil {
				t.Fatalf("claimed a paused Run: %#v, %v", claimed, err)
			}
			resumed := f.CommandRun(run.ID, agentrun.AgentRunCommandResume)
			if resumed.Status != AgentRunStatusQueued || resumed.WakeCondition != nil || resumed.PausedWakeCondition != nil {
				t.Fatalf("resume restored the expired wait: %#v", resumed)
			}
			done := f.DriveToRest(run.ID, 3)
			f.MustStatus(done, AgentRunStatusCompleted)
			tool := f.ToolEntries(run.ID)[0]
			if tool.State != ToolEntryError || !strings.Contains(toolEntryText(tool), string(ToolResultExpired)) {
				t.Fatalf("tool entry = %s %q", tool.State, toolEntryText(tool))
			}
			if replies := f.AgentReplies(conversation.ID); len(replies) != 1 || !strings.HasPrefix(replies[0], "Not closed: ") {
				t.Fatalf("replies = %q", replies)
			}
			if f.Dispatch.Count("update_status") != 0 {
				t.Fatal("an expired call was dispatched")
			}
			actiontest.AssertUnclaimable(t, f.Raw, result.Call, f.Clock.Now().Add(2*time.Hour))
		})
	})
}
