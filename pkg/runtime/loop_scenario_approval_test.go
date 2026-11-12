package runtime

// Approval journeys on the run loop (run-loop section 12 §5.2, tests-T06).
// A reviewed write parks the Run at step end; every decision outcome comes
// back to the model as the governed tool result at the next claim.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

const (
	approvalScenarioSummary = "Close issue 6815"
)

// approvalScenarioRules is the §5.2 script: step 1 proposes closing 6815;
// after the result the model says "Closed." or "Not closed: <content>". A
// changes_requested result is re-proposed once with status wontfix, and a
// message that replaced the approval gets its own reply.
func approvalScenarioRules() []*looptest.ModelRule {
	return []*looptest.ModelRule{
		{Name: "replaced", When: func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "leave it open") },
			Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Leaving 6815 open.") }},
		{Name: "result", When: func(v looptest.StepView) bool {
			_, _, ok := v.ToolResult(looptest.GovernedTool(v, "update_status"))
			return ok
		}, Reply: func(v looptest.StepView) models.ModelStepResult {
			tool := looptest.GovernedTool(v, "update_status")
			content, isError, _ := v.ToolResult(tool)
			switch {
			case !isError:
				return looptest.Say("Closed.")
			case strings.Contains(content, string(models.ToolResultChangesRequested)) && approvalScenarioProposals(v) == 1:
				return looptest.Act("Using wontfix.", looptest.ReviewedCall(tool, map[string]any{"id": "6815", "status": "wontfix"}, "Mark issue 6815 wontfix"))
			}
			return looptest.Say("Not closed: " + content)
		}},
		{Name: "propose", Reply: func(v looptest.StepView) models.ModelStepResult {
			return looptest.Act("Closing the issue.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "closed"}, approvalScenarioSummary))
		}},
	}
}

// approvalScenarioProposals counts the update_status calls the model made.
func approvalScenarioProposals(v looptest.StepView) int {
	tool := looptest.GovernedTool(v, "update_status")
	n := 0
	for _, e := range v.Req.Entries {
		if e.Type == models.SessionEntryTool && e.Tool != nil && e.Tool.Name == tool {
			n++
		}
	}
	return n
}

func newApprovalScenario(t *testing.T, store models.KernelStore, opts ...looptest.Option) *looptest.Fixture {
	t.Helper()
	return looptest.NewFixture(t, store, append([]looptest.Option{looptest.WithApprovalForWrites(), looptest.WithRules(approvalScenarioRules()...)}, opts...)...)
}

func toolEntryText(entry *models.SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil {
		return ""
	}
	return looptest.PartsText(entry.Tool.Result.Content)
}

func logTail(types []string, n int) string {
	if len(types) < n {
		return strings.Join(types, ",")
	}
	return strings.Join(types[len(types)-n:], ",")
}

// assertParkedOnApproval checks the §5.2 park: Run waiting on the single
// pending approval with its lease released, the tool entry awaiting it, the
// action call waiting with loop origin on step 1, and nothing dispatched.
func assertParkedOnApproval(t *testing.T, f *looptest.Fixture, run *models.AgentRun) (*models.ApprovalCheckpoint, *models.ActionCall) {
	t.Helper()
	run = f.MustRun(run.ID)
	if run.Status != models.AgentRunStatusWaitingForApproval || run.LeaseOwner != "" || run.WakeCondition == nil || run.WakeCondition.Type != "approval" {
		t.Fatalf("run = %s lease=%q wake=%#v", run.Status, run.LeaseOwner, run.WakeCondition)
	}
	approvals := f.Approvals(run.ID)
	if len(approvals) != 1 || approvals[0].Status != models.ApprovalStatusPending || approvals[0].ID != run.WakeCondition.Reference {
		t.Fatalf("approvals = %#v (wake %s)", approvals, run.WakeCondition.Reference)
	}
	if approvals[0].Summary != approvalScenarioSummary {
		t.Fatalf("approval title = %q, want the review summary", approvals[0].Summary)
	}
	calls := f.Calls(run.ID)
	if len(calls) != 1 || calls[0].Status != models.ActionCallStatusWaitingApproval || calls[0].Origin != models.ActionOriginLoop || calls[0].ApprovalID != approvals[0].ID {
		t.Fatalf("action calls = %#v", calls)
	}
	steps := f.Steps(run.ID)
	if len(steps) == 0 || calls[0].TurnID != steps[0].ID || steps[0].Status != models.AgentTurnStatusCompleted {
		t.Fatalf("step rows = %#v, call turn %s", steps, calls[0].TurnID)
	}
	tools := f.ToolEntries(run.ID)
	if len(tools) != 1 || tools[0].State != models.ToolEntryAwaitingApproval || tools[0].ActionCallID != calls[0].ID {
		t.Fatalf("tool entries = %#v", tools)
	}
	if got := logTail(f.EntryTypes(run.ID), 4); got != "user,note,assistant,tool:awaiting_approval" {
		t.Fatalf("log tail = %s (log %v)", got, f.EntryTypes(run.ID))
	}
	if f.Dispatch.Count("update_status") != 0 {
		t.Fatal("the write ran before approval")
	}
	return approvals[0], calls[0]
}

// ---------------------------------------------------------------------------

// TestLoopApprovalParksAtStepEnd: the proposal commits call + approval with
// the Run unchanged, and the loop parks at step end. A crash anywhere between
// the proposal and the park leaves a Run the next claim re-parks on the same
// approval: no second card, no dispatch (SPEC A5, section 03 Decision issue 1).
func TestLoopApprovalParksAtStepEnd(t *testing.T) {
	eachStoreOpener(t, "loop_approval_park", func(t *testing.T, open func(*testing.T) models.KernelStore) {
		t.Run("clean", func(t *testing.T) {
			f := newApprovalScenario(t, open(t))
			conversation, run := f.StartChat(looptest.ApprovalScenarioTrigger)
			exit, parked := f.RunClaim(run.ID)
			if exit.Reason != runloop.RunLoopExitParked || exit.Err != nil {
				t.Fatalf("exit = %s, %v", exit.Reason, exit.Err)
			}
			assertParkedOnApproval(t, f, parked)
			if replies := f.AgentReplies(conversation.ID); len(replies) != 0 {
				t.Fatalf("a parked Run posted %q", replies)
			}
		})
		for _, point := range []string{"after_proposal", "at_park"} {
			t.Run("crash_"+point, func(t *testing.T) {
				f := newApprovalScenario(t, open(t))
				hooks := installApprovalHooks(f)
				switch point {
				case "after_proposal":
					hooks.afterProposal = func() { f.Crashes.Arm() }
				case "at_park":
					hooks.beforePark = func() error { f.Crashes.Arm(); return looptest.ErrSimulatedCrash }
				}
				_, run := f.StartChat(looptest.ApprovalScenarioTrigger)
				f.RunClaim(run.ID)
				hooks.clear()
				if approvals := f.Approvals(run.ID); len(approvals) != 1 || approvals[0].Status != models.ApprovalStatusPending {
					t.Fatalf("approvals after crash = %#v", approvals)
				}
				if current := f.MustRun(run.ID); current.Status != models.AgentRunStatusRunning {
					t.Fatalf("run after crash = %s", current.Status)
				}
				f.Crash(nil, nil)
				parked := f.DriveToRest(run.ID, 3)
				approval, _ := assertParkedOnApproval(t, f, parked)
				if parked.LastAppliedTurn == 0 {
					t.Fatalf("re-park applied no step: %#v", parked)
				}
				// The re-parked approval still decides the Run.
				f.Decide(run.ID, models.ApprovalDecisionApprove, "")
				done := f.DriveToRest(run.ID, 3)
				f.MustStatus(done, models.AgentRunStatusCompleted)
				if f.Dispatch.Count("update_status") != 1 {
					t.Fatalf("update_status dispatched %d times, want 1", f.Dispatch.Count("update_status"))
				}
				if got := f.Approvals(run.ID); len(got) != 1 || got[0].ID != approval.ID {
					t.Fatalf("approvals = %#v", got)
				}
			})
		}
	})
}

// TestLoopApprovalDecisionOutcomes is the §5.2 outcome table.
func TestLoopApprovalDecisionOutcomes(t *testing.T) {
	eachStoreOpener(t, "loop_approval_outcomes", func(t *testing.T, open func(*testing.T) models.KernelStore) {
		cases := []struct {
			name     string
			decide   func(f *looptest.Fixture, conversation *models.Conversation, run *models.AgentRun)
			tail     string
			dispatch int
			contains []string
			reply    string
		}{
			{name: "approved", decide: func(f *looptest.Fixture, _ *models.Conversation, run *models.AgentRun) {
				f.Decide(run.ID, models.ApprovalDecisionApprove, "")
			}, tail: "tool:completed,assistant", dispatch: 1, contains: []string{`"status":"closed"`, `"id":"6815"`}, reply: "Closed."},
			{name: "rejected", decide: func(f *looptest.Fixture, _ *models.Conversation, run *models.AgentRun) {
				f.Decide(run.ID, models.ApprovalDecisionReject, "not now")
			}, tail: "tool:error,assistant", contains: []string{"rejected", "not now"}, reply: "Not closed: "},
			{name: "superseded", decide: func(f *looptest.Fixture, conversation *models.Conversation, _ *models.AgentRun) {
				f.Clock.Advance(time.Second)
				f.PostUser(conversation.ID, looptest.FixtureUser, "actually leave it open")
			}, tail: "tool:error,user,assistant", contains: []string{string(models.ToolResultSuperseded), action.ApprovalSupersededReason}, reply: "Leaving 6815 open."},
			{name: "expired", decide: func(f *looptest.Fixture, _ *models.Conversation, run *models.AgentRun) {
				f.Clock.Advance(time.Hour + time.Second)
				approval := f.Approvals(run.ID)[0]
				if _, err := f.Approve.ResolveTimeout(f.Ctx, f.Scope, approval.ID, approval.Revision, approval.ID); err != nil {
					f.T.Fatalf("expire approval: %v", err)
				}
			}, tail: "tool:error,assistant", contains: []string{string(models.ToolResultExpired)}, reply: "Not closed: "},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newApprovalScenario(t, open(t))
				conversation, run := f.ParkedApproval()
				assertParkedOnApproval(t, f, run)
				before := len(f.Model.Calls(models.ModelStepPurposeStep))
				tc.decide(f, conversation, run)
				if queued := f.MustRun(run.ID); queued.Status != models.AgentRunStatusQueued {
					t.Fatalf("run after decision = %s, want queued", queued.Status)
				}
				exit, done := f.RunClaim(run.ID)
				if done.Status != models.AgentRunStatusCompleted {
					t.Fatalf("one claim after the decision ended %s (%s, %v); log %v", done.Status, exit.Reason, exit.Err, f.EntryTypes(run.ID))
				}
				if got := logTail(f.EntryTypes(run.ID), strings.Count(tc.tail, ",")+1); got != tc.tail {
					t.Fatalf("log tail = %s, want %s (log %v)", got, tc.tail, f.EntryTypes(run.ID))
				}
				if got := f.Dispatch.Count("update_status"); got != tc.dispatch {
					t.Fatalf("update_status dispatched %d times, want %d", got, tc.dispatch)
				}
				tool := f.ToolEntries(run.ID)[0]
				for _, want := range tc.contains {
					if !strings.Contains(toolEntryText(tool), want) {
						t.Fatalf("tool result %q does not contain %q", toolEntryText(tool), want)
					}
				}
				// The resumed step saw the result in its path.
				requests := f.Model.Calls(models.ModelStepPurposeStep)
				if len(requests) != before+1 {
					t.Fatalf("model steps after the decision = %d, want 1", len(requests)-before)
				}
				resumed := looptest.StepView{Req: requests[before]}
				if content, _, ok := resumed.ToolResult(looptest.GovernedTool(resumed, "update_status")); !ok || content != toolEntryText(tool) {
					t.Fatalf("resumed step did not see the result: %s", resumed.Describe())
				}
				replies := f.AgentReplies(conversation.ID)
				if len(replies) != 1 || !strings.Contains(replies[0], tc.reply) {
					t.Fatalf("replies = %q, want %q", replies, tc.reply)
				}
				if tc.name == "superseded" {
					approval := f.Approvals(run.ID)[0]
					if approval.Status != models.ApprovalStatusSuperseded || approval.SupersededByMessageID == "" {
						t.Fatalf("approval = %#v", approval)
					}
					if runs := f.ConversationRuns(conversation.ID); len(runs) != 1 {
						t.Fatalf("the replacing message started %d Runs, want 1", len(runs))
					}
				}
			})
		}

		t.Run("changes_requested", func(t *testing.T) {
			f := newApprovalScenario(t, open(t))
			_, run := f.ParkedApproval()
			f.Decide(run.ID, models.ApprovalDecisionRequestChanges, "use status=wontfix")
			if queued := f.MustRun(run.ID); queued.Status != models.AgentRunStatusQueued {
				t.Fatalf("run after decision = %s", queued.Status)
			}
			exit, reparked := f.RunClaim(run.ID)
			if reparked.Status != models.AgentRunStatusWaitingForApproval {
				t.Fatalf("re-proposal did not park: %s (%s, %v) log %v", reparked.Status, exit.Reason, exit.Err, f.EntryTypes(run.ID))
			}
			tools := f.ToolEntries(run.ID)
			if len(tools) != 2 || tools[0].State != models.ToolEntryError || tools[1].State != models.ToolEntryAwaitingApproval {
				t.Fatalf("tool entries = %v", f.EntryTypes(run.ID))
			}
			for _, want := range []string{string(models.ToolResultChangesRequested), "use status=wontfix"} {
				if !strings.Contains(toolEntryText(tools[0]), want) {
					t.Fatalf("result %q does not contain %q", toolEntryText(tools[0]), want)
				}
			}
			if !strings.Contains(string(tools[1].Tool.Arguments), "wontfix") {
				t.Fatalf("re-proposal arguments = %s", tools[1].Tool.Arguments)
			}
			approvals := f.Approvals(run.ID)
			if len(approvals) != 2 || f.Dispatch.Count("update_status") != 0 {
				t.Fatalf("approvals = %d, dispatch = %d", len(approvals), f.Dispatch.Count("update_status"))
			}
			f.Decide(run.ID, models.ApprovalDecisionApprove, "")
			f.MustStatus(f.DriveToRest(run.ID, 3), models.AgentRunStatusCompleted)
			if f.Dispatch.Count("update_status") != 1 {
				t.Fatalf("dispatch = %d", f.Dispatch.Count("update_status"))
			}
		})

		t.Run("canceled", func(t *testing.T) {
			f := newApprovalScenario(t, open(t))
			// The cancel copy answers a trigger that requires a response.
			conversation, run := f.StartChatWith(looptest.ApprovalScenarioTrigger, nil, true)
			f.MustStatus(f.DriveToRest(run.ID, 3), models.AgentRunStatusWaitingForApproval)
			calls := len(f.Model.Calls(""))
			canceled := f.CancelRun(run)
			if canceled.Status != models.AgentRunStatusCanceled {
				t.Fatalf("cancel = %s", canceled.Status)
			}
			tools := f.ToolEntries(run.ID)
			if len(tools) != 1 || tools[0].State != models.ToolEntryError || !strings.Contains(toolEntryText(tools[0]), "canceled") {
				t.Fatalf("open tool entry after cancel: %v %q", f.EntryTypes(run.ID), toolEntryText(tools[0]))
			}
			if approval := f.Approvals(run.ID)[0]; approval.Status != models.ApprovalStatusCanceled {
				t.Fatalf("approval = %s", approval.Status)
			}
			if call := f.Calls(run.ID)[0]; call.Status != models.ActionCallStatusCanceled {
				t.Fatalf("call = %s", call.Status)
			}
			if _, err := f.Scheduler.ReconcileScope(f.Ctx, f.Scope); err != nil {
				t.Fatal(err)
			}
			if replies := f.AgentReplies(conversation.ID); len(replies) != 0 {
				t.Fatalf("the Agent replied after cancel: %q", replies)
			}
			messages := f.Messages(conversation.ID)
			if last := messages[len(messages)-1]; last.Sender.Type != models.ConversationParticipantService || !strings.Contains(last.Content, "canceled before execution") {
				t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
			}
			if len(f.Model.Calls("")) != calls || f.Dispatch.Count("") != 0 {
				t.Fatal("a canceled Run resumed")
			}
			claimed, err := f.Store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{Scope: f.Scope, WorkerID: f.WorkerID, Now: f.Clock.Now(), LeaseDuration: looptest.FixtureLease, AgingInterval: time.Minute})
			if err != nil || claimed != nil {
				t.Fatalf("claim after cancel = %#v, %v", claimed, err)
			}
		})
	})
}

// TestLoopTerminalRunClosesOpenToolEntries: a Run that ends while a tool call
// waits never leaves the chat session with an unanswered call; the next Run
// in the lane renders the closed result (12-tests Decision issue 2).
func TestLoopTerminalRunClosesOpenToolEntries(t *testing.T) {
	looptest.EachStore(t, "loop_terminal_close", func(t *testing.T, store models.KernelStore) {
		f := newApprovalScenario(t, store, looptest.WithRules(&looptest.ModelRule{Name: "next",
			When:  func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "anything else") },
			Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Nothing else.") }}))
		// The "next" rule is after the scenario rules: give it priority.
		f.Model.Mu.Lock()
		rules := f.Model.Rules
		f.Model.Rules = append([]*looptest.ModelRule{rules[len(rules)-1]}, rules[:len(rules)-1]...)
		f.Model.Mu.Unlock()

		conversation, run := f.ParkedApproval()
		f.CancelRun(run)
		for _, entry := range f.ToolEntries(run.ID) {
			if !entry.State.Terminal() {
				t.Fatalf("open tool entry after cancel: %#v", entry)
			}
		}
		f.Clock.Advance(time.Second)
		f.PostUser(conversation.ID, looptest.FixtureUser, "anything else?")
		next := f.ConversationRuns(conversation.ID)
		if len(next) != 2 {
			t.Fatalf("runs = %d", len(next))
		}
		var follow *models.AgentRun
		for _, candidate := range next {
			if candidate.ID != run.ID {
				follow = candidate
			}
		}
		f.MustStatus(f.DriveToRest(follow.ID, 3), models.AgentRunStatusCompleted)
		v := f.LastStepRequest()
		content, isError, ok := v.ToolResult(looptest.GovernedTool(v, "update_status"))
		if !ok || !isError || !strings.Contains(content, "canceled") {
			t.Fatalf("next Run rendered the canceled call as %q (error %v, found %v): %s", content, isError, ok, v.Describe())
		}
	})
}

// TestLoopHeldApprovalDecidedConcurrentlyWithParkAlwaysWakes: a decision that
// lands while the approval is held (entry awaiting, Run still running) or
// right after the park never strands the Run (SPEC §4.3).
func TestLoopHeldApprovalDecidedConcurrentlyWithParkAlwaysWakes(t *testing.T) {
	eachStoreOpener(t, "loop_approval_held", func(t *testing.T, open func(*testing.T) models.KernelStore) {
		for _, tc := range []struct {
			name     string
			decision models.ApprovalDecision
			when     string // held: before the park CAS; parked: right after it
			dispatch int
			reply    string
		}{
			{"held_approve", models.ApprovalDecisionApprove, "held", 1, "Closed."},
			{"held_reject", models.ApprovalDecisionReject, "held", 0, "Not closed: "},
			{"parked_approve", models.ApprovalDecisionApprove, "parked", 1, "Closed."},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newApprovalScenario(t, open(t))
				hooks := installApprovalHooks(f)
				var decided atomic.Bool
				decideOnce := func() {
					if decided.CompareAndSwap(false, true) {
						f.Decide(f.OnlyRunID(), tc.decision, "barrier")
					}
				}
				if tc.when == "held" {
					hooks.beforePark = func() error {
						// The entry is awaiting and the Run still held: the
						// decision must not touch the Run's status.
						run := f.MustRun(f.OnlyRunID())
						if run.Status != models.AgentRunStatusRunning {
							t.Errorf("run at park = %s", run.Status)
						}
						decideOnce()
						if after := f.MustRun(run.ID); after.Status != models.AgentRunStatusRunning || after.Revision <= run.Revision {
							t.Errorf("held decision: status %s revision %d -> %d", after.Status, run.Revision, after.Revision)
						}
						return nil
					}
				} else {
					hooks.afterPark = decideOnce
				}
				conversation, run := f.StartChat(looptest.ApprovalScenarioTrigger)
				done := f.DriveToRest(run.ID, 4)
				hooks.clear()
				f.MustStatus(done, models.AgentRunStatusCompleted)
				if !decided.Load() {
					t.Fatal("the barrier never ran")
				}
				if got := f.Dispatch.Count("update_status"); got != tc.dispatch {
					t.Fatalf("dispatch = %d, want %d", got, tc.dispatch)
				}
				replies := f.AgentReplies(conversation.ID)
				if len(replies) != 1 || !strings.Contains(replies[0], tc.reply) {
					t.Fatalf("replies = %q", replies)
				}
				if approvals := f.Approvals(run.ID); len(approvals) != 1 {
					t.Fatalf("approvals = %d", len(approvals))
				}
			})
		}
	})
}

// TestLoopParkAbortsWhenMessageArrivesDuringStep: a message that arrives
// after the proposal but before the park supersedes the held approval; the
// Run does not park and its next step reads the message.
func TestLoopParkAbortsWhenMessageArrivesDuringStep(t *testing.T) {
	looptest.EachStore(t, "loop_approval_abort", func(t *testing.T, store models.KernelStore) {
		f := newApprovalScenario(t, store)
		hooks := installApprovalHooks(f)
		var conversationID string
		var once sync.Once
		hooks.afterProposal = func() {
			once.Do(func() {
				f.Clock.Advance(time.Second)
				f.PostUser(conversationID, looptest.FixtureUser, "actually leave it open")
			})
		}
		conversation, run := f.StartChatWith(looptest.ApprovalScenarioTrigger, func(c *models.Conversation) { conversationID = c.ID }, false)
		exit, done := f.RunClaim(run.ID)
		hooks.clear()
		if done.Status != models.AgentRunStatusCompleted {
			t.Fatalf("run = %s (%s, %v) log %v", done.Status, exit.Reason, exit.Err, f.EntryTypes(run.ID))
		}
		if got := strings.Join(f.EntryTypes(run.ID), ","); !strings.HasSuffix(got, "user,note,assistant,tool:error,user,assistant") {
			t.Fatalf("log = %s", got)
		}
		approval := f.Approvals(run.ID)[0]
		if approval.Status != models.ApprovalStatusSuperseded {
			t.Fatalf("approval = %s", approval.Status)
		}
		if f.Dispatch.Count("update_status") != 0 {
			t.Fatal("a superseded write ran")
		}
		for _, event := range f.Events(run.ID, "run.transitioned") {
			if to, _ := event.Payload["to"].(string); to == string(models.AgentRunStatusWaitingForApproval) {
				t.Fatalf("the Run parked: %#v", event.Payload)
			}
		}
		replies := f.AgentReplies(conversation.ID)
		if len(replies) != 1 || replies[0] != "Leaving 6815 open." {
			t.Fatalf("replies = %q", replies)
		}
		if runs := f.ConversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("runs = %d", len(runs))
		}
	})
}

// TestLoopApprovedCallExecutesInlineOnTheResumingReplica: the approved call
// runs inside the resuming claim of another replica; a pool worker never
// takes a loop call of a live Run (SPEC A6).
func TestLoopApprovedCallExecutesInlineOnTheResumingReplica(t *testing.T) {
	looptest.EachStore(t, "loop_approval_inline", func(t *testing.T, store models.KernelStore) {
		f := newApprovalScenario(t, store)
		_, run := f.ParkedApproval()
		f.WorkerID = looptest.FixtureWorker2
		f.Build()
		f.Decide(run.ID, models.ApprovalDecisionApprove, "")

		// A pool worker polling while the Run is queued takes nothing.
		claimed, err := f.Store.ClaimNextAction(f.Ctx, models.ActionClaim{Scope: f.Scope, WorkerID: "pool-worker", Now: f.Clock.Now(), LeaseDuration: time.Minute})
		if err != nil || claimed != nil {
			t.Fatalf("pool claimed a loop call of a live Run: %#v, %v", claimed, err)
		}
		var inClaim atomic.Bool
		var dispatchedInClaim atomic.Bool
		f.Dispatch.On("update_status", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			dispatchedInClaim.Store(inClaim.Load())
			return map[string]interface{}{"id": "6815", "status": "closed"}, nil
		})
		inClaim.Store(true)
		_, done := f.RunClaim(run.ID)
		inClaim.Store(false)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if !dispatchedInClaim.Load() || f.Dispatch.Count("update_status") != 1 {
			t.Fatalf("the approved call did not run inside the resuming claim (%d dispatches)", f.Dispatch.Count("update_status"))
		}
		call := f.Calls(run.ID)[0]
		if call.Status != models.ActionCallStatusSucceeded || call.Execution != models.ActionExecutionLoop {
			t.Fatalf("call = %s execution=%s", call.Status, call.Execution)
		}
		if done.LeaseOwner != "" && done.LeaseOwner != looptest.FixtureWorker2 {
			t.Fatalf("lease owner = %s", done.LeaseOwner)
		}
	})
}

// TestLoopReproposalAfterClosedApprovalAsksAgain: after a rejection the
// identical write is a new call that asks again; it is neither dispatched nor
// duplicate-suppressed (D9 suppresses only after success).
func TestLoopReproposalAfterClosedApprovalAsksAgain(t *testing.T) {
	looptest.EachStore(t, "loop_approval_repropose", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithApprovalForWrites(), looptest.WithRules(
			&looptest.ModelRule{Name: "again", When: func(v looptest.StepView) bool {
				content, isErr, ok := v.ToolResult(looptest.GovernedTool(v, "update_status"))
				return ok && isErr && strings.Contains(content, "rejected") && approvalScenarioProposals(v) == 1
			}, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Asking again.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "closed"}, approvalScenarioSummary))
			}},
			&looptest.ModelRule{Name: "propose", Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Closing the issue.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "closed"}, approvalScenarioSummary))
			}},
		))
		_, run := f.ParkedApproval()
		first := f.Approvals(run.ID)[0]
		f.Decide(run.ID, models.ApprovalDecisionReject, "not yet")
		reparked := f.DriveToRest(run.ID, 3)
		f.MustStatus(reparked, models.AgentRunStatusWaitingForApproval)
		tools := f.ToolEntries(run.ID)
		if len(tools) != 2 || tools[0].State != models.ToolEntryError || tools[1].State != models.ToolEntryAwaitingApproval {
			t.Fatalf("log = %v", f.EntryTypes(run.ID))
		}
		if strings.Contains(toolEntryText(tools[1]), string(models.ToolResultDuplicateSuppressed)) {
			t.Fatalf("re-proposal was suppressed: %q", toolEntryText(tools[1]))
		}
		approvals := f.Approvals(run.ID)
		var pending *models.ApprovalCheckpoint
		for _, approval := range approvals {
			if approval.Status == models.ApprovalStatusPending {
				pending = approval
			}
		}
		if len(approvals) != 2 || pending == nil || pending.ID == first.ID || reparked.WakeCondition.Reference != pending.ID {
			t.Fatalf("approvals = %#v", approvals)
		}
		calls := f.Calls(run.ID)
		if len(calls) != 2 || calls[0].ID == calls[1].ID || f.Dispatch.Count("update_status") != 0 {
			t.Fatalf("calls = %d dispatch = %d", len(calls), f.Dispatch.Count("update_status"))
		}
	})
}

// TestLoopApprovalModeIsCarriedOnTheStepRequest replaces
// conversation_approval_mode_test.go:561,591: the per-step capabilities note
// carries the conversation's mode (never the system prompt), and loosening
// the mode approves the pending approval and resumes the Run, whose next step
// sees the new mode as an update.
func TestLoopApprovalModeIsCarriedOnTheStepRequest(t *testing.T) {
	looptest.EachStore(t, "loop_approval_mode", func(t *testing.T, store models.KernelStore) {
		f := newApprovalScenario(t, store)
		conversation, run := f.ParkedApproval()
		if note := stepRequestNoteText(f.Model.Calls(models.ModelStepPurposeStep)[0], models.NoteCapabilities); !strings.Contains(note, "Approval mode: auto.") {
			t.Fatalf("capabilities note lacks the approval mode: %q", note)
		}
		modes, ok := f.Chats.(models.ConversationApprovalModeKernelStore)
		if !ok {
			t.Fatalf("%T has no approval modes", f.Chats)
		}
		service := runtimeconversation.NewConversationApprovalModeService(modes, models.WithClock(f.Clock.Now))
		current, err := f.Convs.GetConversation(f.Ctx, f.Scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.SetApprovalMode(f.Ctx, runtimeconversation.SetConversationApprovalModeRequest{
			Scope: f.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision, Mode: models.ConversationApprovalSkip,
			Actor: models.ActivityActor{Type: "user", ID: looptest.FixtureUser},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.ApprovedApprovalIDs) != 1 || result.ApprovalSweepIncomplete {
			t.Fatalf("sweep = %#v", result)
		}
		f.MustStatus(f.MustRun(run.ID), models.AgentRunStatusQueued)
		f.MustStatus(f.DriveToRest(run.ID, 3), models.AgentRunStatusCompleted)
		if f.Dispatch.Count("update_status") != 1 {
			t.Fatalf("dispatch = %d", f.Dispatch.Count("update_status"))
		}
		resumed := f.LastStepRequest().Req
		var latest string
		for _, entry := range resumed.Entries {
			if entry.Note != nil && entry.Note.Kind == string(models.NoteCapabilities) {
				latest = entry.Note.Text
			}
		}
		if !strings.HasPrefix(latest, "<capabilities_update>") || !strings.Contains(latest, "Approval mode: skip.") {
			t.Fatalf("resumed step's latest capabilities note = %q", latest)
		}
	})
}

// stepRequestNoteText joins the texts of the request's notes of kind.
func stepRequestNoteText(req models.ModelStepRequest, kind models.NoteKind) string {
	var texts []string
	for _, entry := range req.Entries {
		if entry.Note != nil && entry.Note.Kind == string(kind) {
			texts = append(texts, entry.Note.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// TestLoopReviewerGuidanceReachesTheModel ports
// approval_coordinator_test.go:229: the reviewer's guidance is in the tool
// result the next step sees.
func TestLoopReviewerGuidanceReachesTheModel(t *testing.T) {
	looptest.EachStore(t, "loop_approval_guidance", func(t *testing.T, store models.KernelStore) {
		const guidance = "Use the release checklist and mention the owner."
		f := newApprovalScenario(t, store)
		_, run := f.ParkedApproval()
		f.Decide(run.ID, models.ApprovalDecisionRequestChanges, guidance)
		f.DriveToRest(run.ID, 3)
		requests := f.Model.Calls(models.ModelStepPurposeStep)
		if len(requests) < 2 {
			t.Fatalf("model steps = %d", len(requests))
		}
		v := looptest.StepView{Req: requests[1]}
		content, isError, ok := v.ToolResult(looptest.GovernedTool(v, "update_status"))
		if !ok || !isError || !strings.Contains(content, guidance) || !strings.Contains(content, string(models.ToolResultChangesRequested)) {
			t.Fatalf("step 2 tool result = %q (error %v): %s", content, isError, v.Describe())
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers

// approvalHooks is a store wrapper around the fixture's crash store: it runs
// barriers right after an action proposal commits and around the loop's
// approval park transition.
type approvalHooks struct {
	*looptest.CrashStore
	mu            sync.Mutex
	afterProposal func()
	beforePark    func() error
	afterPark     func()
}

func installApprovalHooks(f *looptest.Fixture) *approvalHooks {
	hooks := &approvalHooks{CrashStore: f.Crashes}
	f.Store = hooks
	f.Build()
	return hooks
}

func (h *approvalHooks) clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterProposal, h.beforePark, h.afterPark = nil, nil, nil
}

func (h *approvalHooks) take() (func(), func() error, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.afterProposal, h.beforePark, h.afterPark
}

func (h *approvalHooks) CreateActionProposal(ctx context.Context, proposal models.ActionProposalRecord) (*models.ActionProposalResult, error) {
	result, err := h.CrashStore.CreateActionProposal(ctx, proposal)
	if after, _, _ := h.take(); err == nil && after != nil && proposal.Approval != nil {
		after()
	}
	return result, err
}

func (h *approvalHooks) UpdateAgentRunWithEvent(ctx context.Context, run *models.AgentRun, expectedRevision int64, event *models.ActivityEvent, lease *models.AgentRunLeaseGuard) (*models.ActivityEvent, error) {
	parking := run != nil && run.Status == models.AgentRunStatusWaitingForApproval
	_, before, after := h.take()
	if parking && before != nil {
		if err := before(); err != nil {
			return nil, err
		}
	}
	persisted, err := h.CrashStore.UpdateAgentRunWithEvent(ctx, run, expectedRevision, event, lease)
	if parking && err == nil && after != nil {
		after()
	}
	return persisted, err
}

// eachStoreOpener is looptest.EachStore for scenarios with several subtests per
// store: each subtest opens its own fresh store.
func eachStoreOpener(t *testing.T, prefix string, test func(t *testing.T, open func(*testing.T) models.KernelStore)) {
	t.Helper()
	for _, c := range looptest.StoreMatrix(t, prefix) {
		c := c
		t.Run(c.Name, func(t *testing.T) { test(t, c.Open) })
	}
}

var _ = errors.Is
