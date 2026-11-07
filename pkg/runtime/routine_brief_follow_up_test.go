package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A chat message that answers a pending approval replaces it (the approval
// `superseded` row of 12-tests §5.2). On the loop the outcome is the governed
// tool result and the message a user entry of the same Run; the model then
// decides whether to propose again. These port the old routine-brief
// follow-up tests, whose brief-specific instructions the model now reads
// from the tool result instead.

// supersedeRules re-proposes with the change the replacing message asks for.
func supersedeRules() []*modelRule {
	return []*modelRule{
		{name: "revise", when: func(v stepView) bool {
			content, isError, ok := v.ToolResult(kitGovernedTool(v, "update_status"))
			return ok && isError && strings.Contains(content, string(ToolResultSuperseded)) && strings.Contains(strings.Join(v.UserTexts(), "|"), "wontfix")
		}, reply: func(v stepView) ModelStepResult {
			return act("Using wontfix.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "wontfix"}, "Mark issue 6815 wontfix"))
		}},
	}
}

func newSupersedeScenario(t *testing.T, store KernelStore) *loopFixture {
	t.Helper()
	return newLoopFixture(t, store, withApprovalForWrites(), withRules(supersedeRules()...), withRules(approvalScenarioRules()...))
}

func TestLoopMessageDuringParkedApprovalSupersedesIt(t *testing.T) {
	eachStore(t, "loop_supersede_parked", func(t *testing.T, store KernelStore) {
		f := newSupersedeScenario(t, store)
		conversation, run := f.parkedApproval()
		approval, call := assertParkedOnApproval(t, f, run)

		f.clock.Advance(time.Second)
		message := f.postUser(conversation.ID, loopFixtureUser, "make it wontfix instead")
		if runs := f.conversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("the message started another Run: %d Runs", len(runs))
		}
		queued := f.mustRun(run.ID)
		if queued.Status != AgentRunStatusQueued || queued.WakeCondition != nil {
			t.Fatalf("Run did not resume with the message: %s %#v", queued.Status, queued.WakeCondition)
		}
		got, err := f.raw.GetApproval(f.ctx, f.scope, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != ApprovalStatusSuperseded || got.DecisionReason != ApprovalSupersededReason || got.DecisionBy == nil ||
			*got.DecisionBy != ApprovalSupersededPrincipal || got.DecisionID != "superseded:"+got.ID || got.DecidedAt == nil ||
			got.SupersededByMessageID != message.ID {
			t.Fatalf("approval = %#v", got)
		}
		if outcome := approvalOutcome(got.Status); outcome != "Replaced by your message" {
			t.Fatalf("approval outcome = %q", outcome)
		}
		if current, err := f.raw.GetActionCall(f.ctx, f.scope, call.ID); err != nil || current.Status != ActionCallStatusDenied {
			t.Fatalf("call = %#v, %v", current, err)
		}
		audited := false
		for _, event := range f.events(run.ID, "approval.superseded") {
			audited = audited || event.Actor == (ActivityActor{Type: "system", ID: "kernel"}) && event.CorrelationID == message.ID
		}
		if !audited {
			t.Fatal("the supersede was not audited")
		}

		// The resumed step sees the replaced call's result, then the
		// message, and proposes the change: a new approval, nothing run.
		reparked := f.driveToRest(run.ID, 3)
		f.mustStatus(reparked, AgentRunStatusWaitingForApproval)
		resumed := stepView{req: f.model.calls(ModelStepPurposeStep)[1]}
		if got := resumed.describe(); !strings.Contains(got, "tool:"+kitGovernedTool(resumed, "update_status")+":error user") {
			t.Fatalf("resumed step = %s", got)
		}
		if !strings.HasSuffix(resumed.LastUserText(), "\nmake it wontfix instead") {
			t.Fatalf("resumed step last user = %q", resumed.LastUserText())
		}
		approvals := f.approvals(run.ID)
		if len(approvals) != 2 || reparked.WakeCondition.Reference == approval.ID || f.dispatch.count("update_status") != 0 {
			t.Fatalf("approvals = %d, wake = %#v, dispatch = %d", len(approvals), reparked.WakeCondition, f.dispatch.count("update_status"))
		}
		if replies := f.agentReplies(conversation.ID); len(replies) != 0 {
			t.Fatalf("replies = %q", replies)
		}
	})
}

// Replays and a second quick message converge on one decision; replaying
// them never closes the revised approval proposed after the messages.
func TestLoopApprovalSupersedeIsIdempotent(t *testing.T) {
	eachStore(t, "loop_supersede_idempotent", func(t *testing.T, store KernelStore) {
		f := newSupersedeScenario(t, store)
		conversation, run := f.parkedApproval()
		approval := f.approvals(run.ID)[0]
		f.clock.Advance(time.Second)
		first := f.postUser(conversation.ID, loopFixtureUser, "make it wontfix instead")
		second := f.postUser(conversation.ID, loopFixtureUser, "and mention the owner")
		for _, message := range []*ChannelMessage{first, second, first} {
			if _, _, err := f.scheduler.ScheduleMessage(f.ctx, f.scope, conversation.ID, message.ID); err != nil {
				t.Fatal(err)
			}
		}
		if runs := f.conversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("runs = %d", len(runs))
		}
		if current := f.mustRun(run.ID); current.Status != AgentRunStatusQueued {
			t.Fatalf("run = %s", current.Status)
		}
		resolved, err := f.raw.GetApproval(f.ctx, f.scope, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Status != ApprovalStatusSuperseded || resolved.Revision != approval.Revision+1 || resolved.SupersededByMessageID != first.ID {
			t.Fatalf("approval resolved more than once: %#v", resolved)
		}
		replay, err := f.scheduler.approvals.ResolveSupersededByMessage(f.ctx, f.scope, resolved.ID, approval.Revision, second.ID)
		if err != nil || replay.Resolved {
			t.Fatalf("replayed supersede = %#v, %v", replay, err)
		}

		// One step reads both messages and proposes the revision.
		f.clock.Advance(time.Second)
		reparked := f.driveToRest(run.ID, 3)
		f.mustStatus(reparked, AgentRunStatusWaitingForApproval)
		resumed := stepView{req: f.model.calls(ModelStepPurposeStep)[1]}
		if users := strings.Join(resumed.UserTexts(), "|"); !strings.Contains(users, "wontfix instead|[") || !strings.HasSuffix(users, "\nand mention the owner") {
			t.Fatalf("resumed step users = %q", users)
		}
		revised := reparked.WakeCondition.Reference
		for _, message := range []*ChannelMessage{first, second} {
			if _, _, err := f.scheduler.ScheduleMessage(f.ctx, f.scope, conversation.ID, message.ID); err != nil {
				t.Fatal(err)
			}
		}
		if current, err := f.raw.GetApproval(f.ctx, f.scope, revised); err != nil || current.Status != ApprovalStatusPending {
			t.Fatalf("replay closed the revised approval: %#v, %v", current, err)
		}
		f.mustStatus(f.mustRun(run.ID), AgentRunStatusWaitingForApproval)
	})
}

// A decision that lands first wins; the message is still delivered to the
// same Run and read after the call ran.
func TestLoopApprovalDecidedConcurrentlyWithMessage(t *testing.T) {
	eachStore(t, "loop_supersede_decided", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withApprovalForWrites(), withRules(
			&modelRule{name: "closed-and-asked", when: func(v stepView) bool { return strings.Contains(v.LastUserText(), "is it done") },
				reply: func(v stepView) ModelStepResult {
					if content, isError, ok := v.ToolResult(kitGovernedTool(v, "update_status")); !ok || isError {
						return say("Not closed: " + content)
					}
					return say("Yes, 6815 is closed.")
				}}),
			withRules(approvalScenarioRules()...))
		conversation, run := f.parkedApproval()
		approval := f.approvals(run.ID)[0]
		f.clock.Advance(time.Second)
		message := f.post(conversation.ID, loopFixtureUser, "is it done?")
		f.decide(run.ID, ApprovalDecisionApprove, "")
		// The kernel's supersede of the observed revision loses cleanly.
		if _, err := f.scheduler.approvals.ResolveSupersededByMessage(f.ctx, f.scope, approval.ID, approval.Revision, message.ID); !errors.Is(err, ErrApprovalResolved) {
			t.Fatalf("late supersede error = %v", err)
		}
		if _, _, err := f.scheduler.ScheduleMessage(f.ctx, f.scope, conversation.ID, message.ID); err != nil {
			t.Fatal(err)
		}
		if runs := f.conversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("runs = %d", len(runs))
		}
		if current, err := f.raw.GetApproval(f.ctx, f.scope, approval.ID); err != nil || current.Status != ApprovalStatusApproved || current.DecisionID != "decision-"+approval.ID {
			t.Fatalf("approval = %#v, %v", current, err)
		}
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if f.dispatch.count("update_status") != 1 {
			t.Fatalf("dispatch = %d", f.dispatch.count("update_status"))
		}
		if got := strings.Join(f.entryTypes(run.ID), ","); !strings.HasSuffix(got, "tool:completed,user,assistant") {
			t.Fatalf("log = %s", got)
		}
		if replies := f.agentReplies(conversation.ID); len(replies) != 1 || replies[0] != "Yes, 6815 is closed." {
			t.Fatalf("replies = %q", replies)
		}
	})
}

// Declining is a decision, not a failure: the model reads the rejection and
// the Run completes without a failure explanation.
func TestLoopDeclinedApprovalCompletesCalmly(t *testing.T) {
	eachStore(t, "loop_supersede_declined", func(t *testing.T, store KernelStore) {
		f := newApprovalScenario(t, store)
		conversation, run := f.parkedApproval()
		f.decide(run.ID, ApprovalDecisionReject, "")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if done.FailureExplanation != nil {
			t.Fatalf("a declined approval failed the Run: %#v", done.FailureExplanation)
		}
		if replies := f.agentReplies(conversation.ID); len(replies) != 1 || !strings.HasPrefix(replies[0], "Not closed: ") {
			t.Fatalf("replies = %q", replies)
		}
		if f.dispatch.count("") != 0 {
			t.Fatal("a declined call was dispatched")
		}
	})
}
