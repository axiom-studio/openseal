package runtime

// RunLoop scenario suite (run-loop section 02 §13) on the shared loop kit:
// every scenario drives the real RunLoop with the production bindings
// (loopFixture) over SQLite, plus Postgres when
// OPENSEAL_TEST_POSTGRES_DSN is set. Only the model, the host tools, the
// action dispatcher and the policy are fakes.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// ---------------------------------------------------------------------------
// Scenario helpers

// runClaimWith claims the Run, lets mutate adjust the resolved binding (a
// compactor, a deterministic producer) and drives one claim.
func (f *loopFixture) runClaimWith(ctx context.Context, runID string, mutate func(*agentrun.TurnRunnerBinding)) (RunLoopExit, *AgentRun) {
	f.t.Helper()
	claimed := f.claim(runID)
	binding := f.binding(claimed)
	if binding.Session != nil && claimed.SessionID != binding.Session.ID {
		claimed = f.mustRun(runID)
	}
	if mutate != nil {
		mutate(binding)
	}
	exit := f.loop.Drive(ctx, RunLoopClaim{Run: claimed, WorkerID: f.workerID, Binding: binding})
	return exit, f.mustRun(runID)
}

// withLoopCompactor attaches the production compactor the catalog resolver
// builds: the step host, the session store, the step's own preparer as the
// request builder and the Run activity budget.
func (f *loopFixture) withLoopCompactor(binding *agentrun.TurnRunnerBinding) {
	f.t.Helper()
	built, ok := binding.Compactor.(*LoopCompactor)
	if !ok {
		f.t.Fatalf("binding compactor is %T, want the loop compactor", binding.Compactor)
	}
	activity := NewRunActivityService(f.store, f.store, models.WithClock(f.clock.Now))
	binding.Compactor = &LoopCompactor{
		Host: binding.Host, Sessions: f.store.(AgentSessionStore), Requests: built.Requests,
		Budget: newRunActivityCompactionBudget(f.store, activity, f.clock.Now), Now: f.clock.Now,
	}
}

// startBoundedRun creates an objective Run for finn with a budget.
func (f *loopFixture) startBoundedRun(goal string, budget BudgetPolicy) *AgentRun {
	f.t.Helper()
	created, err := f.portfolio.CreateAgentRun(f.ctx, CreateAgentRunRequest{
		Scope: f.scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: loopFixtureAgent},
		AssignedAgentID: loopFixtureAgent, Goal: goal, Source: RunSourceObjective, Budget: &budget,
	})
	if err != nil {
		f.t.Fatalf("create bounded run: %v", err)
	}
	return created
}

// sortedSteps is the Run's step rows by sequence.
func (f *loopFixture) sortedSteps(runID string) []*AgentTurn {
	f.t.Helper()
	steps := f.steps(runID)
	sort.Slice(steps, func(i, j int) bool { return steps[i].Sequence < steps[j].Sequence })
	return steps
}

// runningStep is the Run's running step row (the loop holds at most one).
func (f *loopFixture) runningStep(runID string) *AgentTurn {
	f.t.Helper()
	var running *AgentTurn
	for _, step := range f.steps(runID) {
		if step.Status == AgentTurnStatusRunning {
			if running != nil {
				f.t.Fatalf("run %s has two running steps: %s and %s", runID, running.ID, step.ID)
			}
			running = step
		}
	}
	return running
}

// entriesOfType counts session entries of one type ("note:<kind>" for notes).
func (f *loopFixture) entriesOfType(runID, kind string) []*SessionEntry {
	f.t.Helper()
	var out []*SessionEntry
	for _, entry := range f.entries(runID) {
		switch {
		case entry.Type == SessionEntryNote && entry.Note != nil && kind == "note:"+string(entry.Note.Kind):
			out = append(out, entry)
		case string(entry.Type) == kind:
			out = append(out, entry)
		}
	}
	return out
}

// lookupRule calls the governed lookup with the step number as the id, so
// repeated steps never trip the repeat guard.
func lookupRule(name string, times int, when func(stepView) bool, before func(context.Context, stepView) error) *modelRule {
	return &modelRule{name: name, times: times, when: when, before: before, reply: func(v stepView) ModelStepResult {
		id := fmt.Sprint(v.Step())
		return act("Looking up issue "+id+".", call(kitGovernedTool(v, "lookup"), map[string]any{"id": id}))
	}}
}

func purposeIs(purpose ModelStepPurpose) func(stepView) bool {
	return func(v stepView) bool { return v.Purpose() == purpose }
}

func stepAtMost(n int) func(stepView) bool {
	return func(v stepView) bool { return v.Purpose() == ModelStepPurposeStep && v.Step() <= n }
}

// stepTransitions are the run.transitioned events that applied stepID.
func (f *loopFixture) stepTransitions(runID, stepID string) []*ActivityEvent {
	f.t.Helper()
	var out []*ActivityEvent
	for _, event := range f.events(runID) {
		if event.TurnID == stepID && strings.HasPrefix(event.EventType, "run.") {
			out = append(out, event)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Steps, narration and previews

func TestRunLoopMultiStepToFinal(t *testing.T) {
	eachStore(t, "rl_multi_step", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			lookupRule("lookup", 2, nil, nil),
			&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
				if len(v.ToolResultsSinceUser()) != 2 {
					t.Errorf("answer step saw %s", v.describe())
				}
				return say("Issues 1 and 2 are open.")
			}},
		))
		conversation, run := f.startChat("check issues 1 and 2")
		exit, done := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFinal || exit.Err != nil || exit.Steps != 3 {
			t.Fatalf("exit = %s steps %d err %v, want final after 3 steps", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(done, AgentRunStatusCompleted)
		if done.LastAppliedTurn != 3 {
			t.Fatalf("LastAppliedTurn = %d, want 3", done.LastAppliedTurn)
		}
		steps := f.sortedSteps(run.ID)
		if len(steps) != 3 {
			t.Fatalf("steps = %d, want 3", len(steps))
		}
		for i, step := range steps {
			if step.Sequence != int64(i+1) || step.Status != AgentTurnStatusCompleted {
				t.Fatalf("step %d = sequence %d status %s, want sequence %d completed", i, step.Sequence, step.Status, i+1)
			}
		}
		if steps[0].OutputSummary != "Looking up issue 1." || steps[1].OutputSummary != "Looking up issue 2." {
			t.Fatalf("narration = %q, %q", steps[0].OutputSummary, steps[1].OutputSummary)
		}
		if steps[2].OutputSummary != "" {
			t.Fatalf("final step narration = %q, want empty", steps[2].OutputSummary)
		}
		if got, want := strings.Join(f.entryTypes(run.ID), ","), "assistant,tool:completed,assistant,tool:completed,assistant"; !strings.HasSuffix(got, want) {
			t.Fatalf("log = %s, want it to end %s", got, want)
		}
		messages := f.messages(conversation.ID)
		if last := messages[len(messages)-1]; last.Sender.ID != loopFixtureAgent || !strings.Contains(last.Content, "Issues 1 and 2 are open.") {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
		if f.dispatch.count("lookup") != 2 {
			t.Fatalf("lookup dispatched %d times, want 2", f.dispatch.count("lookup"))
		}
	})
}

// The step's narration is on its running row before any tool runs, the
// turn.narration event is written with it, and the Run holds one running
// step at a time.
func TestRunLoopNarrationRecordedBeforeTools(t *testing.T) {
	eachStore(t, "rl_narration", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "lookup", times: 1, reply: func(v stepView) ModelStepResult {
				return act("Checking issue 4 first.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "4"}))
			}},
			&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return finalAnswer(map[string]any{"answer": "Issue 4 is open."}) }},
		))
		run := f.startRun(RunKindAgentWork, "Check issue 4")
		var checked atomic.Bool
		f.dispatch.on("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			checked.Store(true)
			step := f.runningStep(run.ID)
			if step == nil {
				t.Errorf("no running step while the tool runs")
			} else if step.OutputSummary != "Checking issue 4 first." {
				t.Errorf("running step narration = %q, want it before the tool runs", step.OutputSummary)
			}
			if len(f.events(run.ID, ActivityEventTurnNarration)) != 1 {
				t.Errorf("turn.narration events before the tool = %d, want 1", len(f.events(run.ID, ActivityEventTurnNarration)))
			}
			current := f.mustRun(run.ID)
			// A second worker's step service, on the same store.
			turns := NewAgentTurnService(f.store, f.store, models.WithClock(f.clock.Now))
			if _, err := turns.BeginTurn(f.ctx, BeginAgentTurnRequest{
				Scope: current.Scope, RunID: current.ID, SessionID: current.SessionID, Producer: StepProducerModel,
				WorkerID: f.workerID, LeaseDuration: loopFixtureLease,
			}); !errors.Is(err, ErrActiveTurnExists) {
				t.Errorf("second BeginTurn while a step runs = %v, want ErrActiveTurnExists", err)
			}
			return map[string]interface{}{"id": "4", "status": "open", "title": "Issue 4"}, nil
		})
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if !checked.Load() {
			t.Fatal("the lookup never ran")
		}
		narration := f.events(run.ID, ActivityEventTurnNarration)
		if len(narration) != 1 || narration[0].Summary != "Checking issue 4 first." || narration[0].TurnID != f.sortedSteps(run.ID)[0].ID {
			t.Fatalf("turn.narration = %#v", narration)
		}
	})
}

func TestRunLoopPreviewResetOnToolStep(t *testing.T) {
	eachStore(t, "rl_preview_reset", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "preview then tool", times: 1,
				before: func(ctx context.Context, v stepView) error {
					if !v.Request().Stream.AnswerPreview {
						t.Errorf("step request does not ask for previews")
					}
					for i, text := range []string{"Issue", "Issue 3 is"} {
						if err := ReportTurnAnswerPreview(ctx, TurnAnswerPreview{AttemptID: "attempt-1", Sequence: uint64(i + 1), Text: text}); err != nil {
							return err
						}
					}
					return nil
				},
				reply: func(v stepView) ModelStepResult {
					return act("Let me check.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "3"}))
				}},
			&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return finalAnswer(map[string]any{"answer": "Issue 3 is open."}) }},
		))
		run := f.startRun(RunKindAgentWork, "Check issue 3")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		previews := f.events(run.ID, "turn.answer_preview")
		if len(previews) != 3 {
			t.Fatalf("previews = %d, want 2 host frames and 1 reset", len(previews))
		}
		reset := previews[2]
		if fmt.Sprint(reset.Payload["reset"]) != "true" || fmt.Sprint(reset.Payload["attemptId"]) != "attempt-1" || fmt.Sprint(reset.Payload["sequence"]) != "3" {
			t.Fatalf("reset preview payload = %#v, want attempt-1 sequence 3 reset", reset.Payload)
		}
		first := f.sortedSteps(run.ID)[0]
		if reset.TurnID != first.ID {
			t.Fatalf("reset is on step %s, want the tool step %s", reset.TurnID, first.ID)
		}
		// The reset follows the step's narration: it is written once the tool
		// calls are recorded.
		narration := f.events(run.ID, ActivityEventTurnNarration)
		if len(narration) != 1 || narration[0].Sequence > reset.Sequence {
			t.Fatalf("narration %#v must precede the reset (sequence %d)", narration, reset.Sequence)
		}
	})
}

// ---------------------------------------------------------------------------
// Fairness

func TestRunLoopFairnessYieldAfter50Steps(t *testing.T) {
	eachStore(t, "rl_yield_steps", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			lookupRule("lookup", 0, stepAtMost(RunLoopMaxStepsPerClaim), nil),
			&modelRule{name: "answer", when: func(v stepView) bool { return v.Step() == RunLoopMaxStepsPerClaim+1 },
				reply: func(stepView) ModelStepResult {
					return finalAnswer(map[string]any{"answer": "All 50 issues are open."})
				}},
		))
		run := f.startRun(RunKindAgentWork, "Check the 50 issues")
		exit, yielded := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitYielded || exit.Err != nil || exit.Steps != RunLoopMaxStepsPerClaim {
			t.Fatalf("first claim = %s after %d steps (%v), want yielded after 50", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(yielded, AgentRunStatusQueued)
		events := f.events(run.ID, "run.yielded")
		if len(events) != 1 || fmt.Sprint(events[0].Payload["steps"]) != "50" {
			t.Fatalf("run.yielded = %#v", events)
		}
		if steps := f.sortedSteps(run.ID); len(steps) != 50 || steps[49].Status != AgentTurnStatusCompleted || f.runningStep(run.ID) != nil {
			t.Fatalf("after the yield: %d steps, running %v", len(steps), f.runningStep(run.ID))
		}
		exit, done := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFinal || exit.Steps != 1 {
			t.Fatalf("second claim = %s after %d steps (%v)", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(done, AgentRunStatusCompleted)
		steps := f.sortedSteps(run.ID)
		if len(steps) != 51 || steps[50].Sequence != 51 || done.LastAppliedTurn != 51 {
			t.Fatalf("steps = %d, last sequence %d, LastAppliedTurn %d; want 51", len(steps), steps[len(steps)-1].Sequence, done.LastAppliedTurn)
		}
		if f.dispatch.count("lookup") != 50 {
			t.Fatalf("lookup dispatched %d times, want 50", f.dispatch.count("lookup"))
		}
	})
}

// Each step takes 50s on the fake clock; the claim yields at the first step
// boundary at or past 8 minutes, never inside a step.
func TestRunLoopFairnessYieldAfterClaimDuration(t *testing.T) {
	eachStore(t, "rl_yield_duration", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		slow := func(ctx context.Context, v stepView) error {
			f.clock.Advance(50 * time.Second)
			// The worker's Run heartbeat, which the fixture does not run.
			_, err := f.raw.RenewAgentRunLease(ctx, f.scope, v.Request().RunID, f.workerID, f.clock.Now(), loopFixtureLease)
			return err
		}
		f = newLoopFixture(t, store, withRules(
			lookupRule("lookup", 0, purposeIs(ModelStepPurposeStep), slow),
		))
		run := f.startRun(RunKindAgentWork, "Keep checking issues")
		exit, yielded := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitYielded || exit.Err != nil {
			t.Fatalf("claim = %s (%v), want yielded", exit.Reason, exit.Err)
		}
		// 9 steps are 450s; the 10th crosses 480s and the claim yields after it.
		if exit.Steps != 10 {
			t.Fatalf("steps in the claim = %d, want 10", exit.Steps)
		}
		f.mustStatus(yielded, AgentRunStatusQueued)
		if len(f.events(run.ID, "run.yielded")) != 1 {
			t.Fatalf("run.yielded events = %d, want 1", len(f.events(run.ID, "run.yielded")))
		}
		steps := f.sortedSteps(run.ID)
		if len(steps) != 10 || yielded.LastAppliedTurn != 10 {
			t.Fatalf("steps = %d, LastAppliedTurn %d; want 10 applied", len(steps), yielded.LastAppliedTurn)
		}
		for _, step := range steps {
			if step.Status != AgentTurnStatusCompleted {
				t.Fatalf("step %d is %s: the yield must come at a step boundary", step.Sequence, step.Status)
			}
		}
		for _, entry := range f.entries(run.ID) {
			if entry.Type == SessionEntryTool && entry.State != ToolEntryCompleted {
				t.Fatalf("tool entry %s is %s at the yield", entry.ID, entry.State)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Parks

// The Run's revision moves while the step runs (the worker heartbeat), so the
// park's CAS conflicts; the loop re-plans the same approval park and moves
// the Run once.
func TestRunLoopParkConflictReplansAndTransitionsOnce(t *testing.T) {
	eachStore(t, "rl_park_replan", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		f = newLoopFixture(t, store, withApprovalForWrites(), withRules(
			&modelRule{name: "update", times: 1,
				before: func(ctx context.Context, v stepView) error {
					_, err := f.raw.RenewAgentRunLease(ctx, f.scope, v.Request().RunID, f.workerID, f.clock.Now(), loopFixtureLease)
					return err
				},
				reply: func(v stepView) ModelStepResult {
					return act("Closing it.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "8", "status": "closed"}, "Close issue 8"))
				}},
			&modelRule{name: "done", reply: func(stepView) ModelStepResult { return finalAnswer(map[string]any{"answer": "Closed issue 8."}) }},
		))
		run := f.startRun(RunKindAgentWork, "Close issue 8")
		exit, parked := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitParked || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want parked", exit.Reason, exit.Err)
		}
		f.mustStatus(parked, AgentRunStatusWaitingForApproval)
		steps := f.sortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != AgentTurnStatusCompleted {
			t.Fatalf("steps = %#v", steps)
		}
		if transitions := f.stepTransitions(run.ID, steps[0].ID); len(transitions) != 1 {
			t.Fatalf("transitions applying the step = %d, want 1", len(transitions))
		}
		if parked.LastAppliedTurn != 1 || parked.WakeCondition == nil || parked.WakeCondition.Type != "approval" {
			t.Fatalf("parked run = applied %d wake %#v", parked.LastAppliedTurn, parked.WakeCondition)
		}
		f.decide(run.ID, ApprovalDecisionApprove, "go ahead")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if f.dispatch.count("update_status") != 1 {
			t.Fatalf("update_status dispatched %d times, want 1", f.dispatch.count("update_status"))
		}
	})
}

// afterInlineTools runs after a step's inline tool phase, before the loop
// parks or settles: the moment a message can land between the approval
// request and the park.
type afterInlineTools struct {
	*claimToolExecutor
	after func()
}

func (x afterInlineTools) ExecuteStepTools(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	out, err := x.claimToolExecutor.ExecuteStepTools(ctx, in)
	if x.after != nil {
		x.after()
	}
	return out, err
}

// A message arrives after the write asked for approval and before the park:
// the park's CAS conflicts, the re-plan supersedes the held approval for the
// new message, and the next step answers it instead of parking.
func TestRunLoopParkReplanSupersededByMessage(t *testing.T) {
	eachStore(t, "rl_park_superseded", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withApprovalForWrites(), withRules(
			&modelRule{name: "update", times: 1, reply: func(v stepView) ModelStepResult {
				return act("Closing it.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "8", "status": "closed"}, "Close issue 8"))
			}},
			&modelRule{name: "leave it", when: func(v stepView) bool { return strings.Contains(v.LastUserText(), "leave issue 8 open") },
				reply: func(stepView) ModelStepResult { return say("Okay, issue 8 stays open.") }},
		))
		conversation, run := f.startChat("close issue 8")
		posted := false
		exit, done := f.runClaimWith(f.ctx, run.ID, func(b *agentrun.TurnRunnerBinding) {
			b.Tools = afterInlineTools{claimToolExecutor: b.Tools.(*claimToolExecutor), after: func() {
				if !posted {
					posted = true
					f.clock.Advance(time.Second) // the message is newer than the approval request
					f.postUser(conversation.ID, loopFixtureUser, "actually, leave issue 8 open")
				}
			}}
		})
		if exit.Reason != RunLoopExitFinal || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want final after 2", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(done, AgentRunStatusCompleted)
		if f.dispatch.count("update_status") != 0 {
			t.Fatal("the superseded write ran")
		}
		approvals, err := f.raw.ListApprovals(f.ctx, ApprovalFilter{Scope: f.scope, RunID: run.ID})
		if err != nil || len(approvals) != 1 || approvals[0].Status == ApprovalStatusPending {
			t.Fatalf("approvals = %#v (%v), want the one request closed", approvals, err)
		}
		steps := f.sortedSteps(run.ID)
		if len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		for _, event := range f.stepTransitions(run.ID, steps[0].ID) {
			if strings.Contains(strings.ToLower(event.Summary), "approval") {
				t.Fatalf("the Run parked for approval: %s", event.Summary)
			}
		}
		messages := f.messages(conversation.ID)
		if last := messages[len(messages)-1]; !strings.Contains(last.Content, "stays open") {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
	})
}

// ---------------------------------------------------------------------------
// Final answers

// Three rejected final tool calls in a row fail the Run final_answer_rejected.
func TestRunLoopFinalToolRejectedThreeTimesFails(t *testing.T) {
	eachStore(t, "rl_final_rejected", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "bad final", reply: func(stepView) ModelStepResult {
			return finalAnswer(map[string]any{"answer": 42})
		}}))
		run := f.startRun(RunKindAgentWork, "Answer badly")
		exit, failed := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFailed || exit.Err != nil || exit.Steps != 3 {
			t.Fatalf("exit = %s after %d steps (%v), want failed after 3", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(failed, AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != "final_answer_rejected" || failed.FailureExplanation.Explained {
			t.Fatalf("failure = %#v, want unexplained final_answer_rejected", failed.FailureExplanation)
		}
		if got := strings.Join(f.entryTypes(run.ID), ","); got != "user,assistant,tool:error,assistant,tool:error,assistant,tool:error" {
			t.Fatalf("log = %s", got)
		}
		if len(f.model.calls(ModelStepPurposeStep)) != 3 {
			t.Fatalf("model steps = %d, want 3", len(f.model.calls(ModelStepPurposeStep)))
		}
	})
}

// An empty plain reply is rejected with a final_rejection note the next step
// reads; the third rejection ends the Run.
func TestRunLoopPlainFinalRejectedThenFails(t *testing.T) {
	eachStore(t, "rl_plain_rejected", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "empty", reply: func(v stepView) ModelStepResult {
			if notes := v.Notes(); v.Step() > 1 && (len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "empty reply")) {
				t.Errorf("step %d does not see the rejection: %s", v.Step(), v.describe())
			}
			return say("")
		}}))
		conversation, run := f.startChat("hello?")
		exit, failed := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFailed || exit.Err != nil || exit.Steps != 3 {
			t.Fatalf("exit = %s after %d steps (%v), want failed after 3", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(failed, AgentRunStatusFailed)
		// The control executor's own limit for empty replies ends the Run
		// at the same third rejection as the loop's bound.
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != "empty_answer" {
			t.Fatalf("failure = %#v, want empty_answer", failed.FailureExplanation)
		}
		if notes := f.entriesOfType(run.ID, "note:"+string(NoteFinalRejection)); len(notes) != 3 {
			t.Fatalf("final_rejection notes = %d, want one per rejected step", len(notes))
		}
		for _, message := range f.messages(conversation.ID)[1:] {
			if message.Sender.ID == loopFixtureAgent && strings.TrimSpace(message.Content) == "" {
				t.Fatalf("an empty reply was posted: %s", kitDescribeMessages(f.messages(conversation.ID)))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Host retry and compaction

func TestRunLoopHostUnavailableRetriesThenFails(t *testing.T) {
	eachStore(t, "rl_host_retry", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "down",
			err: &TurnHostFailure{Code: "host_draining", Message: "The step host is restarting.", Retryable: true}}))
		run := f.startRun(RunKindAgentWork, "Check issue 1")
		// The step host is retried after 2s, 5s and 15s.
		for i, delay := range []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second} {
			exit, requeued := f.runClaim(run.ID)
			if exit.Reason != RunLoopExitRequeued {
				t.Fatalf("claim %d = %s (%v), want requeued", i+1, exit.Reason, exit.Err)
			}
			f.mustStatus(requeued, AgentRunStatusQueued)
			steps := f.sortedSteps(run.ID)
			last := steps[len(steps)-1]
			if last.Status != AgentTurnStatusFailed || last.FailureCode != StepFailureHostUnavailable || last.CompletedAt == nil {
				t.Fatalf("step %d = %s %q", last.Sequence, last.Status, last.FailureCode)
			}
			if !requeued.AvailableAt.Equal(last.CompletedAt.Add(delay)) {
				t.Fatalf("claim %d AvailableAt = %v, want step end + %s", i+1, requeued.AvailableAt, delay)
			}
			f.clock.Advance(delay + time.Millisecond)
			// A new worker counts the retries from the step rows alone.
			f.build()
		}
		exit, failed := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFailed || exit.Err != nil {
			t.Fatalf("fourth claim = %s (%v), want failed", exit.Reason, exit.Err)
		}
		f.mustStatus(failed, AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != StepFailureHostUnavailable || failed.FailureExplanation.Explained {
			t.Fatalf("failure = %#v", failed.FailureExplanation)
		}
		if steps := f.sortedSteps(run.ID); len(steps) != 4 {
			t.Fatalf("steps = %d, want 4", len(steps))
		}
		if got := len(f.model.calls("")); got != 4 {
			t.Fatalf("model calls = %d, want 4 (no explanation)", got)
		}
		if failed.LastAppliedTurn != 4 || failed.BudgetUsage.Turns != 0 {
			t.Fatalf("failed run applied %d, turns %d; host failures charge no Turn", failed.LastAppliedTurn, failed.BudgetUsage.Turns)
		}
	})
}

// bigLookup answers lookups with about 3300 tokens. With five of them the
// overflow cut keeps the last two and a half steps and summarizes the rest.
func bigLookup(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	id, _ := in.Arguments["id"].(string)
	return map[string]interface{}{"id": id, "status": "open", "notes": strings.Repeat("history of issue "+id+" ", 700)}, nil
}

func TestRunLoopContextExceededCompactsOnce(t *testing.T) {
	eachStore(t, "rl_compact_retry", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			lookupRule("lookup", 5, nil, nil),
			&modelRule{name: "too long", times: 1, when: func(v stepView) bool { return v.Purpose() == ModelStepPurposeStep && v.Request().Attempt == 0 },
				err: contextExceeded()},
			&modelRule{name: "summary", when: purposeIs(ModelStepPurposeSummary),
				reply: func(stepView) ModelStepResult { return say("Issues 1 to 3 were looked up; all are open.") }},
			&modelRule{name: "answer", when: func(v stepView) bool { return v.Request().Attempt == 1 }, reply: func(v stepView) ModelStepResult {
				if summary, ok := v.Compaction(); !ok || !strings.Contains(summary, "all are open") {
					t.Errorf("retry does not start from the compaction: %s", v.describe())
				}
				return finalAnswer(map[string]any{"answer": "Issues 1 to 5 are open."})
			}},
		))
		f.dispatch.on("lookup", bigLookup)
		run := f.startRun(RunKindAgentWork, "Check issues 1 to 5")
		exit, done := f.runClaimWith(f.ctx, run.ID, f.withLoopCompactor)
		if exit.Reason != RunLoopExitFinal || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want final", exit.Reason, exit.Err)
		}
		f.mustStatus(done, AgentRunStatusCompleted)
		steps := f.model.calls(ModelStepPurposeStep)
		if len(steps) != 7 {
			t.Fatalf("step calls = %d, want 5 lookups, the rejected attempt and its retry", len(steps))
		}
		rejected, retried := steps[5], steps[6]
		if rejected.StepID != retried.StepID || rejected.Attempt != 0 || retried.Attempt != 1 {
			t.Fatalf("attempts = %s:%d then %s:%d, want one step at attempts 0 and 1", rejected.StepID, rejected.Attempt, retried.StepID, retried.Attempt)
		}
		if summaries := f.model.calls(ModelStepPurposeSummary); len(summaries) != 1 || summaries[0].StepID != rejected.StepID {
			t.Fatalf("summary calls = %d, want 1 for the rejected step", len(summaries))
		}
		if compactions := f.entriesOfType(run.ID, string(SessionEntryCompaction)); len(compactions) != 1 {
			t.Fatalf("compaction entries = %d, want 1", len(compactions))
		}
		if rows := f.sortedSteps(run.ID); len(rows) != 6 || rows[5].ID != rejected.StepID || rows[5].Status != AgentTurnStatusCompleted {
			t.Fatalf("step rows = %d; the retried step must be one completed row", len(rows))
		}
	})
}

func TestRunLoopContextExceededTwiceFails(t *testing.T) {
	eachStore(t, "rl_compact_fail", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			lookupRule("lookup", 5, nil, nil),
			&modelRule{name: "summary", when: purposeIs(ModelStepPurposeSummary),
				reply: func(stepView) ModelStepResult { return say("Issues 1 to 5 were looked up.") }},
			&modelRule{name: "too long", when: purposeIs(ModelStepPurposeStep), err: contextExceeded()},
		))
		f.dispatch.on("lookup", bigLookup)
		run := f.startRun(RunKindAgentWork, "Check issues 1 to 5")
		exit, failed := f.runClaimWith(f.ctx, run.ID, f.withLoopCompactor)
		if exit.Reason != RunLoopExitFailed || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want failed", exit.Reason, exit.Err)
		}
		f.mustStatus(failed, AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Explained {
			t.Fatalf("failure = %#v, want an unexplained failure", failed.FailureExplanation)
		}
		steps := f.model.calls(ModelStepPurposeStep)
		if len(steps) != 7 || steps[5].Attempt != 0 || steps[6].Attempt != 1 || steps[5].StepID != steps[6].StepID {
			t.Fatalf("step calls = %d, want the lookups, attempt 0 and attempt 1 only", len(steps))
		}
		if len(f.model.calls(ModelStepPurposeSummary)) != 1 || len(f.model.calls(ModelStepPurposeExplanation)) != 0 {
			t.Fatalf("summary calls = %d, explanation calls = %d; want 1 and 0",
				len(f.model.calls(ModelStepPurposeSummary)), len(f.model.calls(ModelStepPurposeExplanation)))
		}
		rows := f.sortedSteps(run.ID)
		if last := rows[len(rows)-1]; last.Status != AgentTurnStatusFailed || last.FailureCode != StepFailureContextExceeded {
			t.Fatalf("last step = %s %q, want failed %s", last.Status, last.FailureCode, StepFailureContextExceeded)
		}
	})
}

// ---------------------------------------------------------------------------
// Failure, explanation and credits

func TestRunLoopFailureExplanationStep(t *testing.T) {
	eachStore(t, "rl_explanation", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		var runID string
		f = newLoopFixture(t, store, withRules(
			&modelRule{name: "broken", times: 1, when: purposeIs(ModelStepPurposeStep),
				err: &TurnHostFailure{Code: "model_output_invalid", Message: "The model's reply could not be read."}},
			&modelRule{name: "explain", when: purposeIs(ModelStepPurposeExplanation),
				before: func(context.Context, stepView) error {
					run := f.mustRun(runID)
					if run.Status != AgentRunStatusRunning || run.FailureExplanation == nil || run.FailureExplanation.Explained {
						t.Errorf("before the explanation: status %s failure %#v, want running and unexplained", run.Status, run.FailureExplanation)
					}
					return nil
				},
				reply: func(v stepView) ModelStepResult {
					if len(v.Request().Tools) != 0 {
						t.Errorf("explanation step offers %d tools, want none", len(v.Request().Tools))
					}
					return say("Sorry, I could not read my own reply, so I stopped.")
				}},
		))
		conversation, run := f.startChat("summarize the backlog")
		runID = run.ID
		exit, failed := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFinal || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want final after the explanation", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(failed, AgentRunStatusFailed)
		explanation := failed.FailureExplanation
		if explanation == nil || !explanation.Explained || explanation.Message == "" || failed.Error != explanation.Message {
			t.Fatalf("run error %q explanation %#v, want explained with Error = its message", failed.Error, explanation)
		}
		if len(f.model.calls(ModelStepPurposeExplanation)) != 1 {
			t.Fatalf("explanation calls = %d, want 1", len(f.model.calls(ModelStepPurposeExplanation)))
		}
		steps := f.sortedSteps(run.ID)
		if len(steps) != 2 || steps[0].Status != AgentTurnStatusFailed || steps[1].Status != AgentTurnStatusCompleted {
			t.Fatalf("steps = %d (%s, %s)", len(steps), steps[0].Status, steps[len(steps)-1].Status)
		}
		messages := f.messages(conversation.ID)
		if last := messages[len(messages)-1]; last.Sender.ID != loopFixtureAgent || !strings.Contains(last.Content, "could not read my own reply") {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
	})
}

func TestRunLoopUnexplainableFailureEndsDirectly(t *testing.T) {
	eachStore(t, "rl_unexplainable", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "provider down",
			err: &TurnHostFailure{Code: "provider_unavailable", Message: "The model provider is unavailable."}}))
		_, run := f.startChat("hello")
		exit, failed := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFailed || exit.Err != nil || exit.Steps != 1 {
			t.Fatalf("exit = %s after %d steps (%v), want failed after 1", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(failed, AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Explained || failed.FailureExplanation.Code != "provider_unavailable" {
			t.Fatalf("failure = %#v, want unexplained provider_unavailable", failed.FailureExplanation)
		}
		if got := len(f.model.calls("")); got != 1 {
			t.Fatalf("model calls = %d, want 1", got)
		}
		if failed.CreditError != nil {
			t.Fatalf("credit error = %#v on a provider outage", failed.CreditError)
		}
	})
}

func TestRunLoopCreditErrorFromFailureExplanation(t *testing.T) {
	eachStore(t, "rl_credit_error", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "no credits",
			err: &TurnHostFailure{Code: RunCreditErrorWorkspaceCreditsExhausted, Message: "The workspace has no credits left."}}))
		_, run := f.startChat("hello")
		exit, failed := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitFailed || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want failed", exit.Reason, exit.Err)
		}
		f.mustStatus(failed, AgentRunStatusFailed)
		if failed.CreditError == nil || failed.CreditError.Code != RunCreditErrorWorkspaceCreditsExhausted {
			t.Fatalf("credit error = %#v, failure %#v", failed.CreditError, failed.FailureExplanation)
		}
		if got := len(f.model.calls("")); got != 1 {
			t.Fatalf("model calls = %d, want 1 (credits stop the explanation)", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Budget

// stepReservations counts the Run's step reservations (not the action
// reservations of governed calls, "action:<id>").
func stepReservations(run *AgentRun) int {
	n := 0
	for id := range run.BudgetReservations {
		if !strings.HasPrefix(id, "action:") && !strings.HasPrefix(id, "compaction:") {
			n++
		}
	}
	return n
}

// A bounded Run reserves each step under the step id with Turns 1, settles it
// when the step applies, and pauses when a step uses up the budget.
func TestRunLoopBudgetExhaustionPausesAfterStep(t *testing.T) {
	eachStore(t, "rl_budget_steps", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(lookupRule("lookup", 0, purposeIs(ModelStepPurposeStep), nil)))
		run := f.startBoundedRun("Check issues", BudgetPolicy{MaxTurns: 2})
		var reserved []string
		f.dispatch.on("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			step := f.runningStep(run.ID)
			current := f.mustRun(run.ID)
			if step == nil {
				t.Errorf("no running step during the lookup")
			} else if reservation, ok := current.BudgetReservations[step.ID]; !ok || reservation.Usage.Turns != 1 || stepReservations(current) != 1 {
				t.Errorf("reservations during step %d = %#v, want one step reservation, under the step id with Turns 1", step.Sequence, current.BudgetReservations)
			} else {
				reserved = append(reserved, step.ID)
			}
			return map[string]interface{}{"id": in.Arguments["id"], "status": "open"}, nil
		})
		exit, paused := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitPaused || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want paused after 2", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(paused, AgentRunStatusPaused)
		if paused.BudgetUsage.Turns != 2 || len(paused.BudgetReservations) != 0 || paused.BudgetState != BudgetStateExhausted {
			t.Fatalf("budget = usage %#v reservations %#v state %s", paused.BudgetUsage, paused.BudgetReservations, paused.BudgetState)
		}
		if len(reserved) != 2 {
			t.Fatalf("reserved steps = %v", reserved)
		}
		exhausted := f.events(run.ID, "budget.exhausted")
		if len(exhausted) != 1 || exhausted[0].TurnID != reserved[1] {
			t.Fatalf("budget.exhausted = %#v, want one on the second step", exhausted)
		}
	})
}

func TestRunLoopBudgetAdmissionRefusesModelStep(t *testing.T) {
	eachStore(t, "rl_budget_admission", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(lookupRule("lookup", 0, nil, nil)))
		run := f.startBoundedRun("Check issues", BudgetPolicy{MaxInputTokens: 10})
		exit, paused := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitPaused || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want paused", exit.Reason, exit.Err)
		}
		f.mustStatus(paused, AgentRunStatusPaused)
		steps := f.sortedSteps(run.ID)
		if len(steps) != 1 {
			t.Fatalf("steps = %d, want 1", len(steps))
		}
		step := steps[0]
		if step.Status != AgentTurnStatusCanceled || step.FailureCode != StepFailureBudgetExhausted || step.BudgetAdmission == nil || step.BudgetAdmission.TurnID != step.ID {
			t.Fatalf("step = %s %q admission %#v", step.Status, step.FailureCode, step.BudgetAdmission)
		}
		if got := len(f.model.calls("")); got != 0 {
			t.Fatalf("model calls = %d, want none", got)
		}
		if len(f.events(run.ID, "budget.exhausted")) != 1 || paused.BudgetUsage.Turns != 0 {
			t.Fatalf("budget.exhausted = %d, turns %d", len(f.events(run.ID, "budget.exhausted")), paused.BudgetUsage.Turns)
		}
	})
}

func TestRunLoopDurationBudgetCancelsStep(t *testing.T) {
	eachStore(t, "rl_budget_duration", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "slow",
			before: func(ctx context.Context, _ stepView) error {
				<-ctx.Done()
				return ctx.Err()
			},
			reply: func(v stepView) ModelStepResult {
				return act("Too late.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "1"}))
			}}))
		run := f.startBoundedRun("Check issue 1", BudgetPolicy{MaxDurationMS: 200})
		exit, paused := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitPaused || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want paused", exit.Reason, exit.Err)
		}
		f.mustStatus(paused, AgentRunStatusPaused)
		steps := f.sortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != AgentTurnStatusCanceled || steps[0].FailureCode != StepFailureDurationBudget {
			t.Fatalf("steps = %#v", steps)
		}
		if got := strings.Join(f.entryTypes(run.ID), ","); got != "user" {
			t.Fatalf("log = %s, want only the goal: nothing of the cut step is appended", got)
		}
		if f.dispatch.count("") != 0 {
			t.Fatal("a tool ran after the deadline")
		}
	})
}

// ---------------------------------------------------------------------------
// Cancel and lease loss

func TestRunLoopCanceledRunDiscardsOutput(t *testing.T) {
	eachStore(t, "rl_cancel", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		f = newLoopFixture(t, store, withRules(&modelRule{name: "canceled meanwhile",
			before: func(ctx context.Context, v stepView) error {
				current := f.mustRun(v.Request().RunID)
				_, err := agentrun.NewRunCommandService(f.chats).CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
					Scope: f.scope, RunID: current.ID, ExpectedRevision: current.Revision, Kind: agentrun.AgentRunCommandCancel,
					Actor: ActivityActor{Type: "user", ID: loopFixtureUser}, Summary: "Stop",
				})
				return err
			},
			reply: func(v stepView) ModelStepResult {
				return act("Looking it up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "1"}))
			}}))
		run := f.startRun(RunKindAgentWork, "Check issue 1")
		exit, canceled := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitEnded {
			t.Fatalf("exit = %s (%v), want ended", exit.Reason, exit.Err)
		}
		f.mustStatus(canceled, AgentRunStatusCanceled)
		steps := f.sortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != AgentTurnStatusCanceled || steps[0].FailureCode != agentrun.StepFailureRunEnded {
			t.Fatalf("steps = %#v", steps)
		}
		if assistants := f.entriesOfType(run.ID, string(SessionEntryAssistant)); len(assistants) != 0 {
			t.Fatalf("assistant entries = %d, want the output discarded", len(assistants))
		}
		if f.dispatch.count("") != 0 {
			t.Fatal("a tool ran for a canceled Run")
		}
	})
}

func TestRunLoopLeaseLostWritesNothing(t *testing.T) {
	eachStore(t, "rl_lease_lost", func(t *testing.T, store KernelStore) {
		var f *loopFixture
		var entriesAtSteal int
		var revisionAtSteal int64
		f = newLoopFixture(t, store, withRules(&modelRule{name: "stolen meanwhile",
			before: func(ctx context.Context, v stepView) error {
				f.clock.Advance(loopFixtureLease + time.Second)
				stolen, err := f.raw.ClaimNextAgentRun(ctx, AgentRunClaim{
					Scope: f.scope, WorkerID: loopFixtureWorker2, Now: f.clock.Now(), LeaseDuration: loopFixtureLease, AgingInterval: time.Minute,
				})
				if err != nil {
					return err
				}
				if stolen == nil || stolen.ID != v.Request().RunID {
					t.Errorf("the second worker did not take the Run: %#v", stolen)
					return errors.New("steal failed")
				}
				entriesAtSteal = len(f.entries(stolen.ID))
				revisionAtSteal = stolen.Revision
				return nil
			},
			reply: func(v stepView) ModelStepResult {
				return act("Looking it up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "1"}))
			}}))
		run := f.startRun(RunKindAgentWork, "Check issue 1")
		exit, after := f.runClaim(run.ID)
		if exit.Reason != RunLoopExitLeaseLost {
			t.Fatalf("exit = %s (%v), want lease_lost", exit.Reason, exit.Err)
		}
		if after.Status != AgentRunStatusRunning || after.LeaseOwner != loopFixtureWorker2 || after.Revision != revisionAtSteal {
			t.Fatalf("run after = %s owner %s revision %d, want untouched since the steal (revision %d)",
				after.Status, after.LeaseOwner, after.Revision, revisionAtSteal)
		}
		if got := len(f.entries(run.ID)); got != entriesAtSteal {
			t.Fatalf("session entries %d -> %d after the lease was lost", entriesAtSteal, got)
		}
		if f.dispatch.count("") != 0 {
			t.Fatal("a tool ran after the lease was lost")
		}
		for _, event := range f.events(run.ID) {
			if event.Actor.ID == loopFixtureWorker1 && event.CreatedAt.After(f.clock.Now().Add(-time.Second)) {
				t.Fatalf("worker 1 wrote %s after losing the lease", event.EventType)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Deterministic producers

const scriptedLookupCallID = "det-lookup"

// scriptedRunbookProducer is a deterministic producer: step 1 calls the
// governed lookup, step 2 reads its result from the session and finishes
// (or parks when park is set). Each step writes its engine state.
type scriptedRunbookProducer struct {
	f      *loopFixture
	lookup capability.ModelAction
	park   bool
	steps  []map[string]interface{} // Run.Checkpoint as each step saw it
	result *LoopToolResult
}

func (p *scriptedRunbookProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (p *scriptedRunbookProducer) Produce(ctx context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	p.steps = append(p.steps, maps.Clone(in.Run.Checkpoint))
	if p.park {
		return agentrun.StepOutput{
			Park:        &agentrun.LoopPark{Status: AgentRunStatusWaitingForEvent, Wake: &WakeCondition{Type: "user_message"}, Summary: "Waiting for the requester"},
			KernelState: map[string]interface{}{"phase": "waiting"},
		}, nil
	}
	result, _, err := producerToolResult(ctx, p.f.raw, in.Run, in.Path, scriptedLookupCallID)
	if err != nil {
		return agentrun.StepOutput{}, err
	}
	if result == nil {
		call, err := producerGovernedToolCall(scriptedLookupCallID, &p.lookup, map[string]interface{}{"id": "5"},
			&agentrun.ToolCallDirective{IdempotencyKey: "scripted:" + in.Run.ID + ":lookup", Summary: "Look up issue 5"})
		if err != nil {
			return agentrun.StepOutput{}, err
		}
		return agentrun.StepOutput{ToolCalls: []agentrun.LoopToolCall{call}, KernelState: map[string]interface{}{"phase": "lookup"}}, nil
	}
	p.result = result
	title, _ := result.Output["title"].(string)
	return agentrun.StepOutput{
		Final:       &agentrun.LoopFinal{Answer: title + " is open.", Output: map[string]interface{}{"title": title}},
		KernelState: map[string]interface{}{"phase": "done"},
	}, nil
}

func (f *loopFixture) scriptedRunbook(park bool) *scriptedRunbookProducer {
	f.t.Helper()
	for _, action := range f.offered {
		if action.Action == "lookup" {
			return &scriptedRunbookProducer{f: f, lookup: action, park: park}
		}
	}
	f.t.Fatal("fixture offers no lookup")
	return nil
}

func TestRunLoopDeterministicProducerKernelState(t *testing.T) {
	eachStore(t, "rl_deterministic", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store)
		producer := f.scriptedRunbook(false)
		run := f.startRun(RunKindAgentWork, "Run the issue runbook")
		exit, done := f.runClaimWith(f.ctx, run.ID, func(b *agentrun.TurnRunnerBinding) { b.Producer = producer })
		if exit.Reason != RunLoopExitFinal || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want final after 2", exit.Reason, exit.Steps, exit.Err)
		}
		f.mustStatus(done, AgentRunStatusCompleted)
		if len(f.model.calls("")) != 0 {
			t.Fatal("a deterministic Run called the model")
		}
		if producer.result == nil || producer.result.State != ToolEntryCompleted || producer.result.Output["title"] != "Issue 5" {
			t.Fatalf("second step read %#v, want the lookup result", producer.result)
		}
		if len(producer.steps) != 2 || producer.steps[1]["phase"] != "lookup" {
			t.Fatalf("engine state seen by the steps = %#v, want step 1's state before step 2", producer.steps)
		}
		if done.Checkpoint["phase"] != "done" || done.Output["title"] != "Issue 5" {
			t.Fatalf("run checkpoint %#v output %#v", done.Checkpoint, done.Output)
		}
		steps := f.sortedSteps(run.ID)
		if len(steps) != 2 || steps[0].Producer != StepProducerDeterministic || steps[0].ToolCallCount != 1 {
			t.Fatalf("steps = %#v", steps)
		}
		if f.dispatch.count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.dispatch.count("lookup"))
		}
		if got := strings.Join(f.entryTypes(run.ID), ","); !strings.HasSuffix(got, "tool:completed") || strings.Contains(got, "assistant") {
			t.Fatalf("log = %s, want tool entries only after the goal", got)
		}
	})
}

func TestRunLoopDeterministicParkUsesLoopTransition(t *testing.T) {
	eachStore(t, "rl_deterministic_park", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store)
		producer := f.scriptedRunbook(true)
		run := f.startRun(RunKindAgentWork, "Wait for the requester")
		exit, parked := f.runClaimWith(f.ctx, run.ID, func(b *agentrun.TurnRunnerBinding) { b.Producer = producer })
		if exit.Reason != RunLoopExitParked || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want parked", exit.Reason, exit.Err)
		}
		f.mustStatus(parked, AgentRunStatusWaitingForEvent)
		steps := f.sortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != AgentTurnStatusCompleted {
			t.Fatalf("steps = %#v", steps)
		}
		if transitions := f.stepTransitions(run.ID, steps[0].ID); len(transitions) != 1 {
			t.Fatalf("transitions applying the step = %d, want 1", len(transitions))
		}
		if parked.Checkpoint["phase"] != "waiting" || parked.WakeCondition == nil || parked.WakeCondition.Type != "user_message" || parked.LastAppliedTurn != 1 {
			t.Fatalf("parked run checkpoint %#v wake %#v applied %d", parked.Checkpoint, parked.WakeCondition, parked.LastAppliedTurn)
		}
	})
}
