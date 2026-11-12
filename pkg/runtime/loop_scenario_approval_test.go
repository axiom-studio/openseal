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
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

const (
	approvalScenarioSummary = "Close issue 6815"
)

// approvalScenarioRules is the §5.2 script: step 1 proposes closing 6815;
// after the result the model says "Closed." or "Not closed: <content>". A
// changes_requested result is re-proposed once with status wontfix, and a
// message that replaced the approval gets its own reply.
func approvalScenarioRules() []*modelRule {
	return []*modelRule{
		{name: "replaced", when: func(v stepView) bool { return strings.Contains(v.LastUserText(), "leave it open") },
			reply: func(stepView) ModelStepResult { return say("Leaving 6815 open.") }},
		{name: "result", when: func(v stepView) bool {
			_, _, ok := v.ToolResult(kitGovernedTool(v, "update_status"))
			return ok
		}, reply: func(v stepView) ModelStepResult {
			tool := kitGovernedTool(v, "update_status")
			content, isError, _ := v.ToolResult(tool)
			switch {
			case !isError:
				return say("Closed.")
			case strings.Contains(content, string(ToolResultChangesRequested)) && approvalScenarioProposals(v) == 1:
				return act("Using wontfix.", reviewedCall(tool, map[string]any{"id": "6815", "status": "wontfix"}, "Mark issue 6815 wontfix"))
			}
			return say("Not closed: " + content)
		}},
		{name: "propose", reply: func(v stepView) ModelStepResult {
			return act("Closing the issue.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "closed"}, approvalScenarioSummary))
		}},
	}
}

// approvalScenarioProposals counts the update_status calls the model made.
func approvalScenarioProposals(v stepView) int {
	tool := kitGovernedTool(v, "update_status")
	n := 0
	for _, e := range v.req.Entries {
		if e.Type == SessionEntryTool && e.Tool != nil && e.Tool.Name == tool {
			n++
		}
	}
	return n
}

func newApprovalScenario(t *testing.T, store KernelStore, opts ...loopOption) *loopFixture {
	t.Helper()
	return newLoopFixture(t, store, append([]loopOption{withApprovalForWrites(), withRules(approvalScenarioRules()...)}, opts...)...)
}

func toolEntryText(entry *SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil {
		return ""
	}
	return kitPartsText(entry.Tool.Result.Content)
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
func assertParkedOnApproval(t *testing.T, f *loopFixture, run *AgentRun) (*ApprovalCheckpoint, *ActionCall) {
	t.Helper()
	run = f.mustRun(run.ID)
	if run.Status != AgentRunStatusWaitingForApproval || run.LeaseOwner != "" || run.WakeCondition == nil || run.WakeCondition.Type != "approval" {
		t.Fatalf("run = %s lease=%q wake=%#v", run.Status, run.LeaseOwner, run.WakeCondition)
	}
	approvals := f.approvals(run.ID)
	if len(approvals) != 1 || approvals[0].Status != ApprovalStatusPending || approvals[0].ID != run.WakeCondition.Reference {
		t.Fatalf("approvals = %#v (wake %s)", approvals, run.WakeCondition.Reference)
	}
	if approvals[0].Summary != approvalScenarioSummary {
		t.Fatalf("approval title = %q, want the review summary", approvals[0].Summary)
	}
	calls := f.calls(run.ID)
	if len(calls) != 1 || calls[0].Status != ActionCallStatusWaitingApproval || calls[0].Origin != ActionOriginLoop || calls[0].ApprovalID != approvals[0].ID {
		t.Fatalf("action calls = %#v", calls)
	}
	steps := f.steps(run.ID)
	if len(steps) == 0 || calls[0].TurnID != steps[0].ID || steps[0].Status != AgentTurnStatusCompleted {
		t.Fatalf("step rows = %#v, call turn %s", steps, calls[0].TurnID)
	}
	tools := f.toolEntries(run.ID)
	if len(tools) != 1 || tools[0].State != ToolEntryAwaitingApproval || tools[0].ActionCallID != calls[0].ID {
		t.Fatalf("tool entries = %#v", tools)
	}
	if got := logTail(f.entryTypes(run.ID), 4); got != "user,note,assistant,tool:awaiting_approval" {
		t.Fatalf("log tail = %s (log %v)", got, f.entryTypes(run.ID))
	}
	if f.dispatch.count("update_status") != 0 {
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
	eachStoreOpener(t, "loop_approval_park", func(t *testing.T, open func(*testing.T) KernelStore) {
		t.Run("clean", func(t *testing.T) {
			f := newApprovalScenario(t, open(t))
			conversation, run := f.startChat(approvalScenarioTrigger)
			exit, parked := f.runClaim(run.ID)
			if exit.Reason != runloop.RunLoopExitParked || exit.Err != nil {
				t.Fatalf("exit = %s, %v", exit.Reason, exit.Err)
			}
			assertParkedOnApproval(t, f, parked)
			if replies := f.agentReplies(conversation.ID); len(replies) != 0 {
				t.Fatalf("a parked Run posted %q", replies)
			}
		})
		for _, point := range []string{"after_proposal", "at_park"} {
			t.Run("crash_"+point, func(t *testing.T) {
				f := newApprovalScenario(t, open(t))
				hooks := installApprovalHooks(f)
				switch point {
				case "after_proposal":
					hooks.afterProposal = func() { f.crashes.arm() }
				case "at_park":
					hooks.beforePark = func() error { f.crashes.arm(); return errSimulatedCrash }
				}
				_, run := f.startChat(approvalScenarioTrigger)
				f.runClaim(run.ID)
				hooks.clear()
				if approvals := f.approvals(run.ID); len(approvals) != 1 || approvals[0].Status != ApprovalStatusPending {
					t.Fatalf("approvals after crash = %#v", approvals)
				}
				if current := f.mustRun(run.ID); current.Status != AgentRunStatusRunning {
					t.Fatalf("run after crash = %s", current.Status)
				}
				f.crash(nil, nil)
				parked := f.driveToRest(run.ID, 3)
				approval, _ := assertParkedOnApproval(t, f, parked)
				if parked.LastAppliedTurn == 0 {
					t.Fatalf("re-park applied no step: %#v", parked)
				}
				// The re-parked approval still decides the Run.
				f.decide(run.ID, ApprovalDecisionApprove, "")
				done := f.driveToRest(run.ID, 3)
				f.mustStatus(done, AgentRunStatusCompleted)
				if f.dispatch.count("update_status") != 1 {
					t.Fatalf("update_status dispatched %d times, want 1", f.dispatch.count("update_status"))
				}
				if got := f.approvals(run.ID); len(got) != 1 || got[0].ID != approval.ID {
					t.Fatalf("approvals = %#v", got)
				}
			})
		}
	})
}

// TestLoopApprovalDecisionOutcomes is the §5.2 outcome table.
func TestLoopApprovalDecisionOutcomes(t *testing.T) {
	eachStoreOpener(t, "loop_approval_outcomes", func(t *testing.T, open func(*testing.T) KernelStore) {
		cases := []struct {
			name     string
			decide   func(f *loopFixture, conversation *Conversation, run *AgentRun)
			tail     string
			dispatch int
			contains []string
			reply    string
		}{
			{name: "approved", decide: func(f *loopFixture, _ *Conversation, run *AgentRun) {
				f.decide(run.ID, ApprovalDecisionApprove, "")
			}, tail: "tool:completed,assistant", dispatch: 1, contains: []string{`"status":"closed"`, `"id":"6815"`}, reply: "Closed."},
			{name: "rejected", decide: func(f *loopFixture, _ *Conversation, run *AgentRun) {
				f.decide(run.ID, ApprovalDecisionReject, "not now")
			}, tail: "tool:error,assistant", contains: []string{"rejected", "not now"}, reply: "Not closed: "},
			{name: "superseded", decide: func(f *loopFixture, conversation *Conversation, _ *AgentRun) {
				f.clock.Advance(time.Second)
				f.postUser(conversation.ID, loopFixtureUser, "actually leave it open")
			}, tail: "tool:error,user,assistant", contains: []string{string(ToolResultSuperseded), action.ApprovalSupersededReason}, reply: "Leaving 6815 open."},
			{name: "expired", decide: func(f *loopFixture, _ *Conversation, run *AgentRun) {
				f.clock.Advance(time.Hour + time.Second)
				approval := f.approvals(run.ID)[0]
				if _, err := f.approve.ResolveTimeout(f.ctx, f.scope, approval.ID, approval.Revision, approval.ID); err != nil {
					f.t.Fatalf("expire approval: %v", err)
				}
			}, tail: "tool:error,assistant", contains: []string{string(ToolResultExpired)}, reply: "Not closed: "},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newApprovalScenario(t, open(t))
				conversation, run := f.parkedApproval()
				assertParkedOnApproval(t, f, run)
				before := len(f.model.calls(ModelStepPurposeStep))
				tc.decide(f, conversation, run)
				if queued := f.mustRun(run.ID); queued.Status != AgentRunStatusQueued {
					t.Fatalf("run after decision = %s, want queued", queued.Status)
				}
				exit, done := f.runClaim(run.ID)
				if done.Status != AgentRunStatusCompleted {
					t.Fatalf("one claim after the decision ended %s (%s, %v); log %v", done.Status, exit.Reason, exit.Err, f.entryTypes(run.ID))
				}
				if got := logTail(f.entryTypes(run.ID), strings.Count(tc.tail, ",")+1); got != tc.tail {
					t.Fatalf("log tail = %s, want %s (log %v)", got, tc.tail, f.entryTypes(run.ID))
				}
				if got := f.dispatch.count("update_status"); got != tc.dispatch {
					t.Fatalf("update_status dispatched %d times, want %d", got, tc.dispatch)
				}
				tool := f.toolEntries(run.ID)[0]
				for _, want := range tc.contains {
					if !strings.Contains(toolEntryText(tool), want) {
						t.Fatalf("tool result %q does not contain %q", toolEntryText(tool), want)
					}
				}
				// The resumed step saw the result in its path.
				requests := f.model.calls(ModelStepPurposeStep)
				if len(requests) != before+1 {
					t.Fatalf("model steps after the decision = %d, want 1", len(requests)-before)
				}
				resumed := stepView{req: requests[before]}
				if content, _, ok := resumed.ToolResult(kitGovernedTool(resumed, "update_status")); !ok || content != toolEntryText(tool) {
					t.Fatalf("resumed step did not see the result: %s", resumed.describe())
				}
				replies := f.agentReplies(conversation.ID)
				if len(replies) != 1 || !strings.Contains(replies[0], tc.reply) {
					t.Fatalf("replies = %q, want %q", replies, tc.reply)
				}
				if tc.name == "superseded" {
					approval := f.approvals(run.ID)[0]
					if approval.Status != ApprovalStatusSuperseded || approval.SupersededByMessageID == "" {
						t.Fatalf("approval = %#v", approval)
					}
					if runs := f.conversationRuns(conversation.ID); len(runs) != 1 {
						t.Fatalf("the replacing message started %d Runs, want 1", len(runs))
					}
				}
			})
		}

		t.Run("changes_requested", func(t *testing.T) {
			f := newApprovalScenario(t, open(t))
			_, run := f.parkedApproval()
			f.decide(run.ID, ApprovalDecisionRequestChanges, "use status=wontfix")
			if queued := f.mustRun(run.ID); queued.Status != AgentRunStatusQueued {
				t.Fatalf("run after decision = %s", queued.Status)
			}
			exit, reparked := f.runClaim(run.ID)
			if reparked.Status != AgentRunStatusWaitingForApproval {
				t.Fatalf("re-proposal did not park: %s (%s, %v) log %v", reparked.Status, exit.Reason, exit.Err, f.entryTypes(run.ID))
			}
			tools := f.toolEntries(run.ID)
			if len(tools) != 2 || tools[0].State != ToolEntryError || tools[1].State != ToolEntryAwaitingApproval {
				t.Fatalf("tool entries = %v", f.entryTypes(run.ID))
			}
			for _, want := range []string{string(ToolResultChangesRequested), "use status=wontfix"} {
				if !strings.Contains(toolEntryText(tools[0]), want) {
					t.Fatalf("result %q does not contain %q", toolEntryText(tools[0]), want)
				}
			}
			if !strings.Contains(string(tools[1].Tool.Arguments), "wontfix") {
				t.Fatalf("re-proposal arguments = %s", tools[1].Tool.Arguments)
			}
			approvals := f.approvals(run.ID)
			if len(approvals) != 2 || f.dispatch.count("update_status") != 0 {
				t.Fatalf("approvals = %d, dispatch = %d", len(approvals), f.dispatch.count("update_status"))
			}
			f.decide(run.ID, ApprovalDecisionApprove, "")
			f.mustStatus(f.driveToRest(run.ID, 3), AgentRunStatusCompleted)
			if f.dispatch.count("update_status") != 1 {
				t.Fatalf("dispatch = %d", f.dispatch.count("update_status"))
			}
		})

		t.Run("canceled", func(t *testing.T) {
			f := newApprovalScenario(t, open(t))
			// The cancel copy answers a trigger that requires a response.
			conversation, run := f.startChatWith(approvalScenarioTrigger, nil, true)
			f.mustStatus(f.driveToRest(run.ID, 3), AgentRunStatusWaitingForApproval)
			calls := len(f.model.calls(""))
			canceled := f.cancelRun(run)
			if canceled.Status != AgentRunStatusCanceled {
				t.Fatalf("cancel = %s", canceled.Status)
			}
			tools := f.toolEntries(run.ID)
			if len(tools) != 1 || tools[0].State != ToolEntryError || !strings.Contains(toolEntryText(tools[0]), "canceled") {
				t.Fatalf("open tool entry after cancel: %v %q", f.entryTypes(run.ID), toolEntryText(tools[0]))
			}
			if approval := f.approvals(run.ID)[0]; approval.Status != ApprovalStatusCanceled {
				t.Fatalf("approval = %s", approval.Status)
			}
			if call := f.calls(run.ID)[0]; call.Status != ActionCallStatusCanceled {
				t.Fatalf("call = %s", call.Status)
			}
			if _, err := f.scheduler.ReconcileScope(f.ctx, f.scope); err != nil {
				t.Fatal(err)
			}
			if replies := f.agentReplies(conversation.ID); len(replies) != 0 {
				t.Fatalf("the Agent replied after cancel: %q", replies)
			}
			messages := f.messages(conversation.ID)
			if last := messages[len(messages)-1]; last.Sender.Type != ConversationParticipantService || !strings.Contains(last.Content, "canceled before execution") {
				t.Fatalf("channel = %s", kitDescribeMessages(messages))
			}
			if len(f.model.calls("")) != calls || f.dispatch.count("") != 0 {
				t.Fatal("a canceled Run resumed")
			}
			claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{Scope: f.scope, WorkerID: f.workerID, Now: f.clock.Now(), LeaseDuration: loopFixtureLease, AgingInterval: time.Minute})
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
	eachStore(t, "loop_terminal_close", func(t *testing.T, store KernelStore) {
		f := newApprovalScenario(t, store, withRules(&modelRule{name: "next",
			when:  func(v stepView) bool { return strings.Contains(v.LastUserText(), "anything else") },
			reply: func(stepView) ModelStepResult { return say("Nothing else.") }}))
		// The "next" rule is after the scenario rules: give it priority.
		f.model.mu.Lock()
		rules := f.model.rules
		f.model.rules = append([]*modelRule{rules[len(rules)-1]}, rules[:len(rules)-1]...)
		f.model.mu.Unlock()

		conversation, run := f.parkedApproval()
		f.cancelRun(run)
		for _, entry := range f.toolEntries(run.ID) {
			if !entry.State.Terminal() {
				t.Fatalf("open tool entry after cancel: %#v", entry)
			}
		}
		f.clock.Advance(time.Second)
		f.postUser(conversation.ID, loopFixtureUser, "anything else?")
		next := f.conversationRuns(conversation.ID)
		if len(next) != 2 {
			t.Fatalf("runs = %d", len(next))
		}
		var follow *AgentRun
		for _, candidate := range next {
			if candidate.ID != run.ID {
				follow = candidate
			}
		}
		f.mustStatus(f.driveToRest(follow.ID, 3), AgentRunStatusCompleted)
		v := f.lastStepRequest()
		content, isError, ok := v.ToolResult(kitGovernedTool(v, "update_status"))
		if !ok || !isError || !strings.Contains(content, "canceled") {
			t.Fatalf("next Run rendered the canceled call as %q (error %v, found %v): %s", content, isError, ok, v.describe())
		}
	})
}

// TestLoopHeldApprovalDecidedConcurrentlyWithParkAlwaysWakes: a decision that
// lands while the approval is held (entry awaiting, Run still running) or
// right after the park never strands the Run (SPEC §4.3).
func TestLoopHeldApprovalDecidedConcurrentlyWithParkAlwaysWakes(t *testing.T) {
	eachStoreOpener(t, "loop_approval_held", func(t *testing.T, open func(*testing.T) KernelStore) {
		for _, tc := range []struct {
			name     string
			decision ApprovalDecision
			when     string // held: before the park CAS; parked: right after it
			dispatch int
			reply    string
		}{
			{"held_approve", ApprovalDecisionApprove, "held", 1, "Closed."},
			{"held_reject", ApprovalDecisionReject, "held", 0, "Not closed: "},
			{"parked_approve", ApprovalDecisionApprove, "parked", 1, "Closed."},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newApprovalScenario(t, open(t))
				hooks := installApprovalHooks(f)
				var decided atomic.Bool
				decideOnce := func() {
					if decided.CompareAndSwap(false, true) {
						f.decide(f.onlyRunID(), tc.decision, "barrier")
					}
				}
				if tc.when == "held" {
					hooks.beforePark = func() error {
						// The entry is awaiting and the Run still held: the
						// decision must not touch the Run's status.
						run := f.mustRun(f.onlyRunID())
						if run.Status != AgentRunStatusRunning {
							t.Errorf("run at park = %s", run.Status)
						}
						decideOnce()
						if after := f.mustRun(run.ID); after.Status != AgentRunStatusRunning || after.Revision <= run.Revision {
							t.Errorf("held decision: status %s revision %d -> %d", after.Status, run.Revision, after.Revision)
						}
						return nil
					}
				} else {
					hooks.afterPark = decideOnce
				}
				conversation, run := f.startChat(approvalScenarioTrigger)
				done := f.driveToRest(run.ID, 4)
				hooks.clear()
				f.mustStatus(done, AgentRunStatusCompleted)
				if !decided.Load() {
					t.Fatal("the barrier never ran")
				}
				if got := f.dispatch.count("update_status"); got != tc.dispatch {
					t.Fatalf("dispatch = %d, want %d", got, tc.dispatch)
				}
				replies := f.agentReplies(conversation.ID)
				if len(replies) != 1 || !strings.Contains(replies[0], tc.reply) {
					t.Fatalf("replies = %q", replies)
				}
				if approvals := f.approvals(run.ID); len(approvals) != 1 {
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
	eachStore(t, "loop_approval_abort", func(t *testing.T, store KernelStore) {
		f := newApprovalScenario(t, store)
		hooks := installApprovalHooks(f)
		var conversationID string
		var once sync.Once
		hooks.afterProposal = func() {
			once.Do(func() {
				f.clock.Advance(time.Second)
				f.postUser(conversationID, loopFixtureUser, "actually leave it open")
			})
		}
		conversation, run := f.startChatWith(approvalScenarioTrigger, func(c *Conversation) { conversationID = c.ID }, false)
		exit, done := f.runClaim(run.ID)
		hooks.clear()
		if done.Status != AgentRunStatusCompleted {
			t.Fatalf("run = %s (%s, %v) log %v", done.Status, exit.Reason, exit.Err, f.entryTypes(run.ID))
		}
		if got := strings.Join(f.entryTypes(run.ID), ","); !strings.HasSuffix(got, "user,note,assistant,tool:error,user,assistant") {
			t.Fatalf("log = %s", got)
		}
		approval := f.approvals(run.ID)[0]
		if approval.Status != ApprovalStatusSuperseded {
			t.Fatalf("approval = %s", approval.Status)
		}
		if f.dispatch.count("update_status") != 0 {
			t.Fatal("a superseded write ran")
		}
		for _, event := range f.events(run.ID, "run.transitioned") {
			if to, _ := event.Payload["to"].(string); to == string(AgentRunStatusWaitingForApproval) {
				t.Fatalf("the Run parked: %#v", event.Payload)
			}
		}
		replies := f.agentReplies(conversation.ID)
		if len(replies) != 1 || replies[0] != "Leaving 6815 open." {
			t.Fatalf("replies = %q", replies)
		}
		if runs := f.conversationRuns(conversation.ID); len(runs) != 1 {
			t.Fatalf("runs = %d", len(runs))
		}
	})
}

// TestLoopApprovedCallExecutesInlineOnTheResumingReplica: the approved call
// runs inside the resuming claim of another replica; a pool worker never
// takes a loop call of a live Run (SPEC A6).
func TestLoopApprovedCallExecutesInlineOnTheResumingReplica(t *testing.T) {
	eachStore(t, "loop_approval_inline", func(t *testing.T, store KernelStore) {
		f := newApprovalScenario(t, store)
		_, run := f.parkedApproval()
		f.workerID = loopFixtureWorker2
		f.build()
		f.decide(run.ID, ApprovalDecisionApprove, "")

		// A pool worker polling while the Run is queued takes nothing.
		claimed, err := f.store.ClaimNextAction(f.ctx, ActionClaim{Scope: f.scope, WorkerID: "pool-worker", Now: f.clock.Now(), LeaseDuration: time.Minute})
		if err != nil || claimed != nil {
			t.Fatalf("pool claimed a loop call of a live Run: %#v, %v", claimed, err)
		}
		var inClaim atomic.Bool
		var dispatchedInClaim atomic.Bool
		f.dispatch.on("update_status", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			dispatchedInClaim.Store(inClaim.Load())
			return map[string]interface{}{"id": "6815", "status": "closed"}, nil
		})
		inClaim.Store(true)
		_, done := f.runClaim(run.ID)
		inClaim.Store(false)
		f.mustStatus(done, AgentRunStatusCompleted)
		if !dispatchedInClaim.Load() || f.dispatch.count("update_status") != 1 {
			t.Fatalf("the approved call did not run inside the resuming claim (%d dispatches)", f.dispatch.count("update_status"))
		}
		call := f.calls(run.ID)[0]
		if call.Status != ActionCallStatusSucceeded || call.Execution != ActionExecutionLoop {
			t.Fatalf("call = %s execution=%s", call.Status, call.Execution)
		}
		if done.LeaseOwner != "" && done.LeaseOwner != loopFixtureWorker2 {
			t.Fatalf("lease owner = %s", done.LeaseOwner)
		}
	})
}

// TestLoopReproposalAfterClosedApprovalAsksAgain: after a rejection the
// identical write is a new call that asks again; it is neither dispatched nor
// duplicate-suppressed (D9 suppresses only after success).
func TestLoopReproposalAfterClosedApprovalAsksAgain(t *testing.T) {
	eachStore(t, "loop_approval_repropose", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withApprovalForWrites(), withRules(
			&modelRule{name: "again", when: func(v stepView) bool {
				content, isErr, ok := v.ToolResult(kitGovernedTool(v, "update_status"))
				return ok && isErr && strings.Contains(content, "rejected") && approvalScenarioProposals(v) == 1
			}, reply: func(v stepView) ModelStepResult {
				return act("Asking again.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "closed"}, approvalScenarioSummary))
			}},
			&modelRule{name: "propose", reply: func(v stepView) ModelStepResult {
				return act("Closing the issue.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "closed"}, approvalScenarioSummary))
			}},
		))
		_, run := f.parkedApproval()
		first := f.approvals(run.ID)[0]
		f.decide(run.ID, ApprovalDecisionReject, "not yet")
		reparked := f.driveToRest(run.ID, 3)
		f.mustStatus(reparked, AgentRunStatusWaitingForApproval)
		tools := f.toolEntries(run.ID)
		if len(tools) != 2 || tools[0].State != ToolEntryError || tools[1].State != ToolEntryAwaitingApproval {
			t.Fatalf("log = %v", f.entryTypes(run.ID))
		}
		if strings.Contains(toolEntryText(tools[1]), string(ToolResultDuplicateSuppressed)) {
			t.Fatalf("re-proposal was suppressed: %q", toolEntryText(tools[1]))
		}
		approvals := f.approvals(run.ID)
		var pending *ApprovalCheckpoint
		for _, approval := range approvals {
			if approval.Status == ApprovalStatusPending {
				pending = approval
			}
		}
		if len(approvals) != 2 || pending == nil || pending.ID == first.ID || reparked.WakeCondition.Reference != pending.ID {
			t.Fatalf("approvals = %#v", approvals)
		}
		calls := f.calls(run.ID)
		if len(calls) != 2 || calls[0].ID == calls[1].ID || f.dispatch.count("update_status") != 0 {
			t.Fatalf("calls = %d dispatch = %d", len(calls), f.dispatch.count("update_status"))
		}
	})
}

// TestLoopApprovalModeIsCarriedOnTheStepRequest replaces
// conversation_approval_mode_test.go:561,591: the per-step capabilities note
// carries the conversation's mode (never the system prompt), and loosening
// the mode approves the pending approval and resumes the Run, whose next step
// sees the new mode as an update.
func TestLoopApprovalModeIsCarriedOnTheStepRequest(t *testing.T) {
	eachStore(t, "loop_approval_mode", func(t *testing.T, store KernelStore) {
		f := newApprovalScenario(t, store)
		conversation, run := f.parkedApproval()
		if note := stepRequestNoteText(f.model.calls(ModelStepPurposeStep)[0], NoteCapabilities); !strings.Contains(note, "Approval mode: auto.") {
			t.Fatalf("capabilities note lacks the approval mode: %q", note)
		}
		modes, ok := f.chats.(ConversationApprovalModeKernelStore)
		if !ok {
			t.Fatalf("%T has no approval modes", f.chats)
		}
		service := runtimeconversation.NewConversationApprovalModeService(modes, models.WithClock(f.clock.Now))
		current, err := f.convs.GetConversation(f.ctx, f.scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.SetApprovalMode(f.ctx, runtimeconversation.SetConversationApprovalModeRequest{
			Scope: f.scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision, Mode: ConversationApprovalSkip,
			Actor: ActivityActor{Type: "user", ID: loopFixtureUser},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.ApprovedApprovalIDs) != 1 || result.ApprovalSweepIncomplete {
			t.Fatalf("sweep = %#v", result)
		}
		f.mustStatus(f.mustRun(run.ID), AgentRunStatusQueued)
		f.mustStatus(f.driveToRest(run.ID, 3), AgentRunStatusCompleted)
		if f.dispatch.count("update_status") != 1 {
			t.Fatalf("dispatch = %d", f.dispatch.count("update_status"))
		}
		resumed := f.lastStepRequest().req
		var latest string
		for _, entry := range resumed.Entries {
			if entry.Note != nil && entry.Note.Kind == string(NoteCapabilities) {
				latest = entry.Note.Text
			}
		}
		if !strings.HasPrefix(latest, "<capabilities_update>") || !strings.Contains(latest, "Approval mode: skip.") {
			t.Fatalf("resumed step's latest capabilities note = %q", latest)
		}
	})
}

// stepRequestNoteText joins the texts of the request's notes of kind.
func stepRequestNoteText(req ModelStepRequest, kind NoteKind) string {
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
	eachStore(t, "loop_approval_guidance", func(t *testing.T, store KernelStore) {
		const guidance = "Use the release checklist and mention the owner."
		f := newApprovalScenario(t, store)
		_, run := f.parkedApproval()
		f.decide(run.ID, ApprovalDecisionRequestChanges, guidance)
		f.driveToRest(run.ID, 3)
		requests := f.model.calls(ModelStepPurposeStep)
		if len(requests) < 2 {
			t.Fatalf("model steps = %d", len(requests))
		}
		v := stepView{req: requests[1]}
		content, isError, ok := v.ToolResult(kitGovernedTool(v, "update_status"))
		if !ok || !isError || !strings.Contains(content, guidance) || !strings.Contains(content, string(ToolResultChangesRequested)) {
			t.Fatalf("step 2 tool result = %q (error %v): %s", content, isError, v.describe())
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers

// approvalHooks is a store wrapper around the fixture's crash store: it runs
// barriers right after an action proposal commits and around the loop's
// approval park transition.
type approvalHooks struct {
	*crashStore
	mu            sync.Mutex
	afterProposal func()
	beforePark    func() error
	afterPark     func()
}

func installApprovalHooks(f *loopFixture) *approvalHooks {
	hooks := &approvalHooks{crashStore: f.crashes}
	f.store = hooks
	f.build()
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

func (h *approvalHooks) CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	result, err := h.crashStore.CreateActionProposal(ctx, proposal)
	if after, _, _ := h.take(); err == nil && after != nil && proposal.Approval != nil {
		after()
	}
	return result, err
}

func (h *approvalHooks) UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error) {
	parking := run != nil && run.Status == AgentRunStatusWaitingForApproval
	_, before, after := h.take()
	if parking && before != nil {
		if err := before(); err != nil {
			return nil, err
		}
	}
	persisted, err := h.crashStore.UpdateAgentRunWithEvent(ctx, run, expectedRevision, event, lease)
	if parking && err == nil && after != nil {
		after()
	}
	return persisted, err
}

// eachStoreOpener is eachStore for scenarios with several subtests per
// store: each subtest opens its own fresh store.
func eachStoreOpener(t *testing.T, prefix string, test func(t *testing.T, open func(*testing.T) KernelStore)) {
	t.Helper()
	for _, c := range kernelStoreMatrix(t, prefix) {
		c := c
		t.Run(c.name, func(t *testing.T) { test(t, c.open) })
	}
}

var _ = errors.Is
