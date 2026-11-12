package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
)

// A chat message that answers a pending approval replaces it (the approval
// `superseded` row of 12-tests §5.2). On the loop the outcome is the governed
// tool result and the message a user entry of the same Run; the model then
// decides whether to propose again. These port the old routine-brief
// follow-up tests, whose brief-specific instructions the model now reads
// from the tool result instead.

// supersedeRules re-proposes with the change the replacing message asks for.
func supersedeRules() []*looptest.ModelRule {
	return []*looptest.ModelRule{
		{Name: "revise", When: func(v looptest.StepView) bool {
			content, isError, ok := v.ToolResult(looptest.GovernedTool(v, "update_status"))
			return ok && isError && strings.Contains(content, string(ToolResultSuperseded)) && strings.Contains(strings.Join(v.UserTexts(), "|"), "wontfix")
		}, Reply: func(v looptest.StepView) ModelStepResult {
			return looptest.Act("Using wontfix.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "wontfix"}, "Mark issue 6815 wontfix"))
		}},
	}
}

func newSupersedeScenario(t *testing.T, store KernelStore) *looptest.Fixture {
	t.Helper()
	return looptest.NewFixture(t, store, looptest.WithApprovalForWrites(), looptest.WithRules(supersedeRules()...), looptest.WithRules(approvalScenarioRules()...))
}

func TestLoopMessageDuringParkedApprovalSupersedesIt(t *testing.T) {
	looptest.EachStore(t, "loop_supersede_parked", func(t *testing.T, store KernelStore) {
		f := newSupersedeScenario(t, store)
		conversation, run := f.ParkedApproval()
		approval, call := assertParkedOnApproval(t, f, run)

		f.Clock.Advance(time.Second)
		message := f.PostUser(conversation.ID, looptest.FixtureUser, "make it wontfix instead")
		if runs := f.ConversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("the message started another Run: %d Runs", len(runs))
		}
		queued := f.MustRun(run.ID)
		if queued.Status != AgentRunStatusQueued || queued.WakeCondition != nil {
			t.Fatalf("Run did not resume with the message: %s %#v", queued.Status, queued.WakeCondition)
		}
		got, err := f.Raw.GetApproval(f.Ctx, f.Scope, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != ApprovalStatusSuperseded || got.DecisionReason != action.ApprovalSupersededReason || got.DecisionBy == nil ||
			*got.DecisionBy != action.ApprovalSupersededPrincipal || got.DecisionID != "superseded:"+got.ID || got.DecidedAt == nil ||
			got.SupersededByMessageID != message.ID {
			t.Fatalf("approval = %#v", got)
		}
		if title := action.PresentApproval(got).Title; !strings.HasPrefix(title, "Replaced by your message: ") {
			t.Fatalf("approval outcome = %q", title)
		}
		if current, err := f.Raw.GetActionCall(f.Ctx, f.Scope, call.ID); err != nil || current.Status != ActionCallStatusDenied {
			t.Fatalf("call = %#v, %v", current, err)
		}
		audited := false
		for _, event := range f.Events(run.ID, "approval.superseded") {
			audited = audited || event.Actor == (ActivityActor{Type: "system", ID: "kernel"}) && event.CorrelationID == message.ID
		}
		if !audited {
			t.Fatal("the supersede was not audited")
		}

		// The resumed step sees the replaced call's result, then the
		// message, and proposes the change: a new approval, nothing run.
		reparked := f.DriveToRest(run.ID, 3)
		f.MustStatus(reparked, AgentRunStatusWaitingForApproval)
		resumed := looptest.StepView{Req: f.Model.Calls(ModelStepPurposeStep)[1]}
		if got := resumed.Describe(); !strings.Contains(got, "tool:"+looptest.GovernedTool(resumed, "update_status")+":error user") {
			t.Fatalf("resumed step = %s", got)
		}
		if !strings.HasSuffix(resumed.LastUserText(), "\nmake it wontfix instead") {
			t.Fatalf("resumed step last user = %q", resumed.LastUserText())
		}
		approvals := f.Approvals(run.ID)
		if len(approvals) != 2 || reparked.WakeCondition.Reference == approval.ID || f.Dispatch.Count("update_status") != 0 {
			t.Fatalf("approvals = %d, wake = %#v, dispatch = %d", len(approvals), reparked.WakeCondition, f.Dispatch.Count("update_status"))
		}
		if replies := f.AgentReplies(conversation.ID); len(replies) != 0 {
			t.Fatalf("replies = %q", replies)
		}
	})
}

// Replays and a second quick message converge on one decision; replaying
// them never closes the revised approval proposed after the messages.
func TestLoopApprovalSupersedeIsIdempotent(t *testing.T) {
	looptest.EachStore(t, "loop_supersede_idempotent", func(t *testing.T, store KernelStore) {
		f := newSupersedeScenario(t, store)
		conversation, run := f.ParkedApproval()
		approval := f.Approvals(run.ID)[0]
		f.Clock.Advance(time.Second)
		first := f.PostUser(conversation.ID, looptest.FixtureUser, "make it wontfix instead")
		second := f.PostUser(conversation.ID, looptest.FixtureUser, "and mention the owner")
		for _, message := range []*ChannelMessage{first, second, first} {
			if _, _, err := f.Scheduler.ScheduleMessage(f.Ctx, f.Scope, conversation.ID, message.ID); err != nil {
				t.Fatal(err)
			}
		}
		if runs := f.ConversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("runs = %d", len(runs))
		}
		if current := f.MustRun(run.ID); current.Status != AgentRunStatusQueued {
			t.Fatalf("run = %s", current.Status)
		}
		resolved, err := f.Raw.GetApproval(f.Ctx, f.Scope, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != ApprovalStatusSuperseded || resolved.Revision != approval.Revision+1 || resolved.SupersededByMessageID != first.ID {
			t.Fatalf("approval resolved more than once: %#v", resolved)
		}
		replay, err := schedulerApprovals(f).ResolveSupersededByMessage(f.Ctx, f.Scope, resolved.ID, approval.Revision, second.ID)
		if err != nil || replay.Resolved {
			t.Fatalf("replayed supersede = %#v, %v", replay, err)
		}

		// One step reads both messages and proposes the revision.
		f.Clock.Advance(time.Second)
		reparked := f.DriveToRest(run.ID, 3)
		f.MustStatus(reparked, AgentRunStatusWaitingForApproval)
		resumed := looptest.StepView{Req: f.Model.Calls(ModelStepPurposeStep)[1]}
		if users := strings.Join(resumed.UserTexts(), "|"); !strings.Contains(users, "wontfix instead|[") || !strings.HasSuffix(users, "\nand mention the owner") {
			t.Fatalf("resumed step users = %q", users)
		}
		revised := reparked.WakeCondition.Reference
		for _, message := range []*ChannelMessage{first, second} {
			if _, _, err := f.Scheduler.ScheduleMessage(f.Ctx, f.Scope, conversation.ID, message.ID); err != nil {
				t.Fatal(err)
			}
		}
		if current, err := f.Raw.GetApproval(f.Ctx, f.Scope, revised); err != nil || current.Status != ApprovalStatusPending {
			t.Fatalf("replay closed the revised approval: %#v, %v", current, err)
		}
		f.MustStatus(f.MustRun(run.ID), AgentRunStatusWaitingForApproval)
	})
}

// A decision that lands first wins; the message is still delivered to the
// same Run and read after the call ran.
func TestLoopApprovalDecidedConcurrentlyWithMessage(t *testing.T) {
	looptest.EachStore(t, "loop_supersede_decided", func(t *testing.T, store KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithApprovalForWrites(), looptest.WithRules(
			&looptest.ModelRule{Name: "closed-and-asked", When: func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "is it done") },
				Reply: func(v looptest.StepView) ModelStepResult {
					if content, isError, ok := v.ToolResult(looptest.GovernedTool(v, "update_status")); !ok || isError {
						return looptest.Say("Not closed: " + content)
					}
					return looptest.Say("Yes, 6815 is closed.")
				}}),
			looptest.WithRules(approvalScenarioRules()...))
		conversation, run := f.ParkedApproval()
		approval := f.Approvals(run.ID)[0]
		f.Clock.Advance(time.Second)
		message := f.Post(conversation.ID, looptest.FixtureUser, "is it done?")
		f.Decide(run.ID, ApprovalDecisionApprove, "")
		// The kernel's supersede of the observed revision loses cleanly.
		if _, err := schedulerApprovals(f).ResolveSupersededByMessage(f.Ctx, f.Scope, approval.ID, approval.Revision, message.ID); !errors.Is(err, ErrApprovalResolved) {
			t.Fatalf("late supersede error = %v", err)
		}
		if _, _, err := f.Scheduler.ScheduleMessage(f.Ctx, f.Scope, conversation.ID, message.ID); err != nil {
			t.Fatal(err)
		}
		if runs := f.ConversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("runs = %d", len(runs))
		}
		if current, err := f.Raw.GetApproval(f.Ctx, f.Scope, approval.ID); err != nil || current.Status != ApprovalStatusApproved || current.DecisionID != "decision-"+approval.ID {
			t.Fatalf("approval = %#v, %v", current, err)
		}
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, AgentRunStatusCompleted)
		if f.Dispatch.Count("update_status") != 1 {
			t.Fatalf("dispatch = %d", f.Dispatch.Count("update_status"))
		}
		if got := strings.Join(f.EntryTypes(run.ID), ","); !strings.HasSuffix(got, "tool:completed,user,assistant") {
			t.Fatalf("log = %s", got)
		}
		if replies := f.AgentReplies(conversation.ID); len(replies) != 1 || replies[0] != "Yes, 6815 is closed." {
			t.Fatalf("replies = %q", replies)
		}
	})
}

// Declining is a decision, not a failure: the model reads the rejection and
// the Run completes without a failure explanation.
func TestLoopDeclinedApprovalCompletesCalmly(t *testing.T) {
	looptest.EachStore(t, "loop_supersede_declined", func(t *testing.T, store KernelStore) {
		f := newApprovalScenario(t, store)
		conversation, run := f.ParkedApproval()
		f.Decide(run.ID, ApprovalDecisionReject, "")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, AgentRunStatusCompleted)
		if done.FailureExplanation != nil {
			t.Fatalf("a declined approval failed the Run: %#v", done.FailureExplanation)
		}
		if replies := f.AgentReplies(conversation.ID); len(replies) != 1 || !strings.HasPrefix(replies[0], "Not closed: ") {
			t.Fatalf("replies = %q", replies)
		}
		if f.Dispatch.Count("") != 0 {
			t.Fatal("a declined call was dispatched")
		}
	})
}

// schedulerApprovals is the approval coordinator the fixture's scheduler
// builds: the chat store with no authorizer, on the fixture clock.
func schedulerApprovals(f *looptest.Fixture) *action.ApprovalCoordinator {
	return action.NewApprovalCoordinator(f.Chats, f.Chats, nil, models.WithClock(f.Clock.Now))
}
