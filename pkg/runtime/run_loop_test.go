package runtime

// RunLoop scenario suite (run-loop section 02 §13) on the shared loop kit:
// every scenario drives the real RunLoop with the production bindings
// (looptest.Fixture) over SQLite, plus Postgres when
// OPENSEAL_TEST_POSTGRES_DSN is set. Only the model, the host tools, the
// action dispatcher and the policy are fakes.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

// ---------------------------------------------------------------------------
// Scenario helpers

// lookupRule calls the governed lookup with the step number as the id, so
// repeated steps never trip the repeat guard.
func lookupRule(name string, times int, when func(looptest.StepView) bool, before func(context.Context, looptest.StepView) error) *looptest.ModelRule {
	return &looptest.ModelRule{Name: name, Times: times, When: when, Before: before, Reply: func(v looptest.StepView) models.ModelStepResult {
		id := fmt.Sprint(v.Step())
		return looptest.Act("Looking up issue "+id+".", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": id}))
	}}
}

func purposeIs(purpose models.ModelStepPurpose) func(looptest.StepView) bool {
	return func(v looptest.StepView) bool { return v.Purpose() == purpose }
}

func stepAtMost(n int) func(looptest.StepView) bool {
	return func(v looptest.StepView) bool { return v.Purpose() == models.ModelStepPurposeStep && v.Step() <= n }
}

// ---------------------------------------------------------------------------
// Steps, narration and previews

func TestRunLoopMultiStepToFinal(t *testing.T) {
	looptest.EachStore(t, "rl_multi_step", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			lookupRule("lookup", 2, nil, nil),
			&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
				if len(v.ToolResultsSinceUser()) != 2 {
					t.Errorf("answer step saw %s", v.Describe())
				}
				return looptest.Say("Issues 1 and 2 are open.")
			}},
		))
		conversation, run := f.StartChat("check issues 1 and 2")
		exit, done := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFinal || exit.Err != nil || exit.Steps != 3 {
			t.Fatalf("exit = %s steps %d err %v, want final after 3 steps", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if done.LastAppliedTurn != 3 {
			t.Fatalf("LastAppliedTurn = %d, want 3", done.LastAppliedTurn)
		}
		steps := f.SortedSteps(run.ID)
		if len(steps) != 3 {
			t.Fatalf("steps = %d, want 3", len(steps))
		}
		for i, step := range steps {
			if step.Sequence != int64(i+1) || step.Status != models.AgentTurnStatusCompleted {
				t.Fatalf("step %d = sequence %d status %s, want sequence %d completed", i, step.Sequence, step.Status, i+1)
			}
		}
		if steps[0].OutputSummary != "Looking up issue 1." || steps[1].OutputSummary != "Looking up issue 2." {
			t.Fatalf("narration = %q, %q", steps[0].OutputSummary, steps[1].OutputSummary)
		}
		if steps[2].OutputSummary != "" {
			t.Fatalf("final step narration = %q, want empty", steps[2].OutputSummary)
		}
		if got, want := strings.Join(f.EntryTypes(run.ID), ","), "assistant,tool:completed,assistant,tool:completed,assistant"; !strings.HasSuffix(got, want) {
			t.Fatalf("log = %s, want it to end %s", got, want)
		}
		messages := f.Messages(conversation.ID)
		if last := messages[len(messages)-1]; last.Sender.ID != looptest.FixtureAgent || !strings.Contains(last.Content, "Issues 1 and 2 are open.") {
			t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
		}
		if f.Dispatch.Count("lookup") != 2 {
			t.Fatalf("lookup dispatched %d times, want 2", f.Dispatch.Count("lookup"))
		}
	})
}

// The step's narration is on its running row before any tool runs, the
// turn.narration event is written with it, and the Run holds one running
// step at a time.
func TestRunLoopNarrationRecordedBeforeTools(t *testing.T) {
	looptest.EachStore(t, "rl_narration", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking issue 4 first.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "4"}))
			}},
			&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Issue 4 is open."})
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Check issue 4")
		var checked atomic.Bool
		f.Dispatch.On("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			checked.Store(true)
			step := f.RunningStep(run.ID)
			if step == nil {
				t.Errorf("no running step while the tool runs")
			} else if step.OutputSummary != "Checking issue 4 first." {
				t.Errorf("running step narration = %q, want it before the tool runs", step.OutputSummary)
			}
			if len(f.Events(run.ID, activity.ActivityEventTurnNarration)) != 1 {
				t.Errorf("turn.narration events before the tool = %d, want 1", len(f.Events(run.ID, activity.ActivityEventTurnNarration)))
			}
			current := f.MustRun(run.ID)
			// A second worker's step service, on the same store.
			turns := activity.NewAgentTurnService(f.Store, f.Store, models.WithClock(f.Clock.Now))
			if _, err := turns.BeginTurn(f.Ctx, activity.BeginAgentTurnRequest{
				Scope: current.Scope, RunID: current.ID, SessionID: current.SessionID, Producer: models.StepProducerModel,
				WorkerID: f.WorkerID, LeaseDuration: looptest.FixtureLease,
			}); !errors.Is(err, models.ErrActiveTurnExists) {
				t.Errorf("second BeginTurn while a step runs = %v, want ErrActiveTurnExists", err)
			}
			return map[string]interface{}{"id": "4", "status": "open", "title": "Issue 4"}, nil
		})
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if !checked.Load() {
			t.Fatal("the lookup never ran")
		}
		narration := f.Events(run.ID, activity.ActivityEventTurnNarration)
		if len(narration) != 1 || narration[0].Summary != "Checking issue 4 first." || narration[0].TurnID != f.SortedSteps(run.ID)[0].ID {
			t.Fatalf("turn.narration = %#v", narration)
		}
	})
}

func TestRunLoopPreviewResetOnToolStep(t *testing.T) {
	looptest.EachStore(t, "rl_preview_reset", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "preview then tool", Times: 1,
				Before: func(ctx context.Context, v looptest.StepView) error {
					if !v.Request().Stream.AnswerPreview {
						t.Errorf("step request does not ask for previews")
					}
					for i, text := range []string{"Issue", "Issue 3 is"} {
						if err := models.ReportTurnAnswerPreview(ctx, models.TurnAnswerPreview{AttemptID: "attempt-1", Sequence: uint64(i + 1), Text: text}); err != nil {
							return err
						}
					}
					return nil
				},
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Let me check.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "3"}))
				}},
			&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Issue 3 is open."})
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Check issue 3")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		previews := f.Events(run.ID, "turn.answer_preview")
		if len(previews) != 3 {
			t.Fatalf("previews = %d, want 2 host frames and 1 reset", len(previews))
		}
		reset := previews[2]
		if fmt.Sprint(reset.Payload["reset"]) != "true" || fmt.Sprint(reset.Payload["attemptId"]) != "attempt-1" || fmt.Sprint(reset.Payload["sequence"]) != "3" {
			t.Fatalf("reset preview payload = %#v, want attempt-1 sequence 3 reset", reset.Payload)
		}
		first := f.SortedSteps(run.ID)[0]
		if reset.TurnID != first.ID {
			t.Fatalf("reset is on step %s, want the tool step %s", reset.TurnID, first.ID)
		}
		// The reset follows the step's narration: it is written once the tool
		// calls are recorded.
		narration := f.Events(run.ID, activity.ActivityEventTurnNarration)
		if len(narration) != 1 || narration[0].Sequence > reset.Sequence {
			t.Fatalf("narration %#v must precede the reset (sequence %d)", narration, reset.Sequence)
		}
	})
}

// ---------------------------------------------------------------------------
// Fairness

func TestRunLoopFairnessYieldAfter50Steps(t *testing.T) {
	looptest.EachStore(t, "rl_yield_steps", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			lookupRule("lookup", 0, stepAtMost(runloop.RunLoopMaxStepsPerClaim), nil),
			&looptest.ModelRule{Name: "answer", When: func(v looptest.StepView) bool { return v.Step() == runloop.RunLoopMaxStepsPerClaim+1 },
				Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": "All 50 issues are open."})
				}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Check the 50 issues")
		exit, yielded := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitYielded || exit.Err != nil || exit.Steps != runloop.RunLoopMaxStepsPerClaim {
			t.Fatalf("first claim = %s after %d steps (%v), want yielded after 50", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(yielded, models.AgentRunStatusQueued)
		events := f.Events(run.ID, "run.yielded")
		if len(events) != 1 || fmt.Sprint(events[0].Payload["steps"]) != "50" {
			t.Fatalf("run.yielded = %#v", events)
		}
		if steps := f.SortedSteps(run.ID); len(steps) != 50 || steps[49].Status != models.AgentTurnStatusCompleted || f.RunningStep(run.ID) != nil {
			t.Fatalf("after the yield: %d steps, running %v", len(steps), f.RunningStep(run.ID))
		}
		exit, done := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFinal || exit.Steps != 1 {
			t.Fatalf("second claim = %s after %d steps (%v)", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(done, models.AgentRunStatusCompleted)
		steps := f.SortedSteps(run.ID)
		if len(steps) != 51 || steps[50].Sequence != 51 || done.LastAppliedTurn != 51 {
			t.Fatalf("steps = %d, last sequence %d, LastAppliedTurn %d; want 51", len(steps), steps[len(steps)-1].Sequence, done.LastAppliedTurn)
		}
		if f.Dispatch.Count("lookup") != 50 {
			t.Fatalf("lookup dispatched %d times, want 50", f.Dispatch.Count("lookup"))
		}
	})
}

// Each step takes 50s on the fake clock; the claim yields at the first step
// boundary at or past 8 minutes, never inside a step.
func TestRunLoopFairnessYieldAfterClaimDuration(t *testing.T) {
	looptest.EachStore(t, "rl_yield_duration", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		slow := func(ctx context.Context, v looptest.StepView) error {
			f.Clock.Advance(50 * time.Second)
			// The worker's Run heartbeat, which the fixture does not run.
			_, err := f.Raw.RenewAgentRunLease(ctx, f.Scope, v.Request().RunID, f.WorkerID, f.Clock.Now(), looptest.FixtureLease)
			return err
		}
		f = looptest.NewFixture(t, store, looptest.WithRules(
			lookupRule("lookup", 0, purposeIs(models.ModelStepPurposeStep), slow),
		))
		run := f.StartRun(models.RunKindAgentWork, "Keep checking issues")
		exit, yielded := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitYielded || exit.Err != nil {
			t.Fatalf("claim = %s (%v), want yielded", exit.Reason, exit.Err)
		}
		// 9 steps are 450s; the 10th crosses 480s and the claim yields after it.
		if exit.Steps != 10 {
			t.Fatalf("steps in the claim = %d, want 10", exit.Steps)
		}
		f.MustStatus(yielded, models.AgentRunStatusQueued)
		if len(f.Events(run.ID, "run.yielded")) != 1 {
			t.Fatalf("run.yielded events = %d, want 1", len(f.Events(run.ID, "run.yielded")))
		}
		steps := f.SortedSteps(run.ID)
		if len(steps) != 10 || yielded.LastAppliedTurn != 10 {
			t.Fatalf("steps = %d, LastAppliedTurn %d; want 10 applied", len(steps), yielded.LastAppliedTurn)
		}
		for _, step := range steps {
			if step.Status != models.AgentTurnStatusCompleted {
				t.Fatalf("step %d is %s: the yield must come at a step boundary", step.Sequence, step.Status)
			}
		}
		for _, entry := range f.Entries(run.ID) {
			if entry.Type == models.SessionEntryTool && entry.State != models.ToolEntryCompleted {
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
	looptest.EachStore(t, "rl_park_replan", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		f = looptest.NewFixture(t, store, looptest.WithApprovalForWrites(), looptest.WithRules(
			&looptest.ModelRule{Name: "update", Times: 1,
				Before: func(ctx context.Context, v looptest.StepView) error {
					_, err := f.Raw.RenewAgentRunLease(ctx, f.Scope, v.Request().RunID, f.WorkerID, f.Clock.Now(), looptest.FixtureLease)
					return err
				},
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Closing it.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "8", "status": "closed"}, "Close issue 8"))
				}},
			&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Closed issue 8."})
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Close issue 8")
		exit, parked := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitParked || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want parked", exit.Reason, exit.Err)
		}
		f.MustStatus(parked, models.AgentRunStatusWaitingForApproval)
		steps := f.SortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != models.AgentTurnStatusCompleted {
			t.Fatalf("steps = %#v", steps)
		}
		if transitions := f.StepTransitions(run.ID, steps[0].ID); len(transitions) != 1 {
			t.Fatalf("transitions applying the step = %d, want 1", len(transitions))
		}
		if parked.LastAppliedTurn != 1 || parked.WakeCondition == nil || parked.WakeCondition.Type != "approval" {
			t.Fatalf("parked run = applied %d wake %#v", parked.LastAppliedTurn, parked.WakeCondition)
		}
		f.Decide(run.ID, models.ApprovalDecisionApprove, "go ahead")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if f.Dispatch.Count("update_status") != 1 {
			t.Fatalf("update_status dispatched %d times, want 1", f.Dispatch.Count("update_status"))
		}
	})
}

// afterInlineTools runs after a step's inline tool phase, before the loop
// parks or settles: the moment a message can land between the approval
// request and the park.
type afterInlineTools struct {
	*runloop.ClaimToolExecutor
	after func()
}

func (x afterInlineTools) ExecuteStepTools(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	out, err := x.ClaimToolExecutor.ExecuteStepTools(ctx, in)
	if x.after != nil {
		x.after()
	}
	return out, err
}

// A message arrives after the write asked for approval and before the park:
// the park's CAS conflicts, the re-plan supersedes the held approval for the
// new message, and the next step answers it instead of parking.
func TestRunLoopParkReplanSupersededByMessage(t *testing.T) {
	looptest.EachStore(t, "rl_park_superseded", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithApprovalForWrites(), looptest.WithRules(
			&looptest.ModelRule{Name: "update", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Closing it.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "8", "status": "closed"}, "Close issue 8"))
			}},
			&looptest.ModelRule{Name: "leave it", When: func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "leave issue 8 open") },
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Okay, issue 8 stays open.") }},
		))
		conversation, run := f.StartChat("close issue 8")
		posted := false
		exit, done := f.RunClaimWith(f.Ctx, run.ID, func(b *agentrun.TurnRunnerBinding) {
			b.Tools = afterInlineTools{ClaimToolExecutor: b.Tools.(*runloop.ClaimToolExecutor), after: func() {
				if !posted {
					posted = true
					f.Clock.Advance(time.Second) // the message is newer than the approval request
					f.PostUser(conversation.ID, looptest.FixtureUser, "actually, leave issue 8 open")
				}
			}}
		})
		if exit.Reason != runloop.RunLoopExitFinal || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want final after 2", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if f.Dispatch.Count("update_status") != 0 {
			t.Fatal("the superseded write ran")
		}
		approvals, err := f.Raw.ListApprovals(f.Ctx, models.ApprovalFilter{Scope: f.Scope, RunID: run.ID})
		if err != nil || len(approvals) != 1 || approvals[0].Status == models.ApprovalStatusPending {
			t.Fatalf("approvals = %#v (%v), want the one request closed", approvals, err)
		}
		steps := f.SortedSteps(run.ID)
		if len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		for _, event := range f.StepTransitions(run.ID, steps[0].ID) {
			if strings.Contains(strings.ToLower(event.Summary), "approval") {
				t.Fatalf("the Run parked for approval: %s", event.Summary)
			}
		}
		messages := f.Messages(conversation.ID)
		if last := messages[len(messages)-1]; !strings.Contains(last.Content, "stays open") {
			t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
		}
	})
}

// ---------------------------------------------------------------------------
// Final answers

// Three rejected final tool calls in a row fail the Run final_answer_rejected.
func TestRunLoopFinalToolRejectedThreeTimesFails(t *testing.T) {
	looptest.EachStore(t, "rl_final_rejected", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "bad final", Reply: func(looptest.StepView) models.ModelStepResult {
			return looptest.FinalAnswer(map[string]any{"answer": 42})
		}}))
		run := f.StartRun(models.RunKindAgentWork, "Answer badly")
		exit, failed := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFailed || exit.Err != nil || exit.Steps != 3 {
			t.Fatalf("exit = %s after %d steps (%v), want failed after 3", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(failed, models.AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != "final_answer_rejected" || failed.FailureExplanation.Explained {
			t.Fatalf("failure = %#v, want unexplained final_answer_rejected", failed.FailureExplanation)
		}
		if got := strings.Join(f.EntryTypes(run.ID), ","); got != "user,assistant,tool:error,assistant,tool:error,assistant,tool:error" {
			t.Fatalf("log = %s", got)
		}
		if len(f.Model.Calls(models.ModelStepPurposeStep)) != 3 {
			t.Fatalf("model steps = %d, want 3", len(f.Model.Calls(models.ModelStepPurposeStep)))
		}
	})
}

// An empty plain reply is rejected with a final_rejection note the next step
// reads; the third rejection ends the Run.
func TestRunLoopPlainFinalRejectedThenFails(t *testing.T) {
	looptest.EachStore(t, "rl_plain_rejected", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "empty", Reply: func(v looptest.StepView) models.ModelStepResult {
			if notes := v.Notes(); v.Step() > 1 && (len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "empty reply")) {
				t.Errorf("step %d does not see the rejection: %s", v.Step(), v.Describe())
			}
			return looptest.Say("")
		}}))
		conversation, run := f.StartChat("hello?")
		exit, failed := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFailed || exit.Err != nil || exit.Steps != 3 {
			t.Fatalf("exit = %s after %d steps (%v), want failed after 3", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(failed, models.AgentRunStatusFailed)
		// The control executor's own limit for empty replies ends the Run
		// at the same third rejection as the loop's bound.
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != "empty_answer" {
			t.Fatalf("failure = %#v, want empty_answer", failed.FailureExplanation)
		}
		if notes := f.EntriesOfType(run.ID, "note:"+string(models.NoteFinalRejection)); len(notes) != 3 {
			t.Fatalf("final_rejection notes = %d, want one per rejected step", len(notes))
		}
		for _, message := range f.Messages(conversation.ID)[1:] {
			if message.Sender.ID == looptest.FixtureAgent && strings.TrimSpace(message.Content) == "" {
				t.Fatalf("an empty reply was posted: %s", looptest.DescribeMessages(f.Messages(conversation.ID)))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Host retry and compaction

func TestRunLoopHostUnavailableRetriesThenFails(t *testing.T) {
	looptest.EachStore(t, "rl_host_retry", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "down",
			Err: &modelstep.TurnHostFailure{Code: "host_draining", Message: "The step host is restarting.", Retryable: true}}))
		run := f.StartRun(models.RunKindAgentWork, "Check issue 1")
		// The step host is retried after 2s, 5s and 15s.
		for i, delay := range []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second} {
			exit, requeued := f.RunClaim(run.ID)
			if exit.Reason != runloop.RunLoopExitRequeued {
				t.Fatalf("claim %d = %s (%v), want requeued", i+1, exit.Reason, exit.Err)
			}
			f.MustStatus(requeued, models.AgentRunStatusQueued)
			steps := f.SortedSteps(run.ID)
			last := steps[len(steps)-1]
			if last.Status != models.AgentTurnStatusFailed || last.FailureCode != runloop.StepFailureHostUnavailable || last.CompletedAt == nil {
				t.Fatalf("step %d = %s %q", last.Sequence, last.Status, last.FailureCode)
			}
			if !requeued.AvailableAt.Equal(last.CompletedAt.Add(delay)) {
				t.Fatalf("claim %d AvailableAt = %v, want step end + %s", i+1, requeued.AvailableAt, delay)
			}
			f.Clock.Advance(delay + time.Millisecond)
			// A new worker counts the retries from the step rows alone.
			f.Build()
		}
		exit, failed := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFailed || exit.Err != nil {
			t.Fatalf("fourth claim = %s (%v), want failed", exit.Reason, exit.Err)
		}
		f.MustStatus(failed, models.AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != runloop.StepFailureHostUnavailable || failed.FailureExplanation.Explained {
			t.Fatalf("failure = %#v", failed.FailureExplanation)
		}
		if steps := f.SortedSteps(run.ID); len(steps) != 4 {
			t.Fatalf("steps = %d, want 4", len(steps))
		}
		if got := len(f.Model.Calls("")); got != 4 {
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
	looptest.EachStore(t, "rl_compact_retry", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			lookupRule("lookup", 5, nil, nil),
			&looptest.ModelRule{Name: "too long", Times: 1, When: func(v looptest.StepView) bool {
				return v.Purpose() == models.ModelStepPurposeStep && v.Request().Attempt == 0
			},
				Err: contextExceeded()},
			&looptest.ModelRule{Name: "summary", When: purposeIs(models.ModelStepPurposeSummary),
				Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Say("Issues 1 to 3 were looked up; all are open.")
				}},
			&looptest.ModelRule{Name: "answer", When: func(v looptest.StepView) bool { return v.Request().Attempt == 1 }, Reply: func(v looptest.StepView) models.ModelStepResult {
				if summary, ok := v.Compaction(); !ok || !strings.Contains(summary, "all are open") {
					t.Errorf("retry does not start from the compaction: %s", v.Describe())
				}
				return looptest.FinalAnswer(map[string]any{"answer": "Issues 1 to 5 are open."})
			}},
		))
		f.Dispatch.On("lookup", bigLookup)
		run := f.StartRun(models.RunKindAgentWork, "Check issues 1 to 5")
		exit, done := f.RunClaimWith(f.Ctx, run.ID, f.WithLoopCompactor)
		if exit.Reason != runloop.RunLoopExitFinal || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want final", exit.Reason, exit.Err)
		}
		f.MustStatus(done, models.AgentRunStatusCompleted)
		steps := f.Model.Calls(models.ModelStepPurposeStep)
		if len(steps) != 7 {
			t.Fatalf("step calls = %d, want 5 lookups, the rejected attempt and its retry", len(steps))
		}
		rejected, retried := steps[5], steps[6]
		if rejected.StepID != retried.StepID || rejected.Attempt != 0 || retried.Attempt != 1 {
			t.Fatalf("attempts = %s:%d then %s:%d, want one step at attempts 0 and 1", rejected.StepID, rejected.Attempt, retried.StepID, retried.Attempt)
		}
		if summaries := f.Model.Calls(models.ModelStepPurposeSummary); len(summaries) != 1 || summaries[0].StepID != rejected.StepID {
			t.Fatalf("summary calls = %d, want 1 for the rejected step", len(summaries))
		}
		if compactions := f.EntriesOfType(run.ID, string(models.SessionEntryCompaction)); len(compactions) != 1 {
			t.Fatalf("compaction entries = %d, want 1", len(compactions))
		}
		if rows := f.SortedSteps(run.ID); len(rows) != 6 || rows[5].ID != rejected.StepID || rows[5].Status != models.AgentTurnStatusCompleted {
			t.Fatalf("step rows = %d; the retried step must be one completed row", len(rows))
		}
	})
}

func TestRunLoopContextExceededTwiceFails(t *testing.T) {
	looptest.EachStore(t, "rl_compact_fail", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			lookupRule("lookup", 5, nil, nil),
			&looptest.ModelRule{Name: "summary", When: purposeIs(models.ModelStepPurposeSummary),
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Issues 1 to 5 were looked up.") }},
			&looptest.ModelRule{Name: "too long", When: purposeIs(models.ModelStepPurposeStep), Err: contextExceeded()},
		))
		f.Dispatch.On("lookup", bigLookup)
		run := f.StartRun(models.RunKindAgentWork, "Check issues 1 to 5")
		exit, failed := f.RunClaimWith(f.Ctx, run.ID, f.WithLoopCompactor)
		if exit.Reason != runloop.RunLoopExitFailed || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want failed", exit.Reason, exit.Err)
		}
		f.MustStatus(failed, models.AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Explained {
			t.Fatalf("failure = %#v, want an unexplained failure", failed.FailureExplanation)
		}
		steps := f.Model.Calls(models.ModelStepPurposeStep)
		if len(steps) != 7 || steps[5].Attempt != 0 || steps[6].Attempt != 1 || steps[5].StepID != steps[6].StepID {
			t.Fatalf("step calls = %d, want the lookups, attempt 0 and attempt 1 only", len(steps))
		}
		if len(f.Model.Calls(models.ModelStepPurposeSummary)) != 1 || len(f.Model.Calls(models.ModelStepPurposeExplanation)) != 0 {
			t.Fatalf("summary calls = %d, explanation calls = %d; want 1 and 0",
				len(f.Model.Calls(models.ModelStepPurposeSummary)), len(f.Model.Calls(models.ModelStepPurposeExplanation)))
		}
		rows := f.SortedSteps(run.ID)
		if last := rows[len(rows)-1]; last.Status != models.AgentTurnStatusFailed || last.FailureCode != runloop.StepFailureContextExceeded {
			t.Fatalf("last step = %s %q, want failed %s", last.Status, last.FailureCode, runloop.StepFailureContextExceeded)
		}
	})
}

// ---------------------------------------------------------------------------
// Failure, explanation and credits

func TestRunLoopFailureExplanationStep(t *testing.T) {
	looptest.EachStore(t, "rl_explanation", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		var runID string
		f = looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "broken", Times: 1, When: purposeIs(models.ModelStepPurposeStep),
				Err: &modelstep.TurnHostFailure{Code: "model_output_invalid", Message: "The model's reply could not be read."}},
			&looptest.ModelRule{Name: "explain", When: purposeIs(models.ModelStepPurposeExplanation),
				Before: func(context.Context, looptest.StepView) error {
					run := f.MustRun(runID)
					if run.Status != models.AgentRunStatusRunning || run.FailureExplanation == nil || run.FailureExplanation.Explained {
						t.Errorf("before the explanation: status %s failure %#v, want running and unexplained", run.Status, run.FailureExplanation)
					}
					return nil
				},
				Reply: func(v looptest.StepView) models.ModelStepResult {
					if len(v.Request().Tools) != 0 {
						t.Errorf("explanation step offers %d tools, want none", len(v.Request().Tools))
					}
					return looptest.Say("Sorry, I could not read my own reply, so I stopped.")
				}},
		))
		conversation, run := f.StartChat("summarize the backlog")
		runID = run.ID
		exit, failed := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFinal || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want final after the explanation", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(failed, models.AgentRunStatusFailed)
		explanation := failed.FailureExplanation
		if explanation == nil || !explanation.Explained || explanation.Message == "" || failed.Error != explanation.Message {
			t.Fatalf("run error %q explanation %#v, want explained with Error = its message", failed.Error, explanation)
		}
		if len(f.Model.Calls(models.ModelStepPurposeExplanation)) != 1 {
			t.Fatalf("explanation calls = %d, want 1", len(f.Model.Calls(models.ModelStepPurposeExplanation)))
		}
		steps := f.SortedSteps(run.ID)
		if len(steps) != 2 || steps[0].Status != models.AgentTurnStatusFailed || steps[1].Status != models.AgentTurnStatusCompleted {
			t.Fatalf("steps = %d (%s, %s)", len(steps), steps[0].Status, steps[len(steps)-1].Status)
		}
		messages := f.Messages(conversation.ID)
		if last := messages[len(messages)-1]; last.Sender.ID != looptest.FixtureAgent || !strings.Contains(last.Content, "could not read my own reply") {
			t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
		}
	})
}

func TestRunLoopUnexplainableFailureEndsDirectly(t *testing.T) {
	looptest.EachStore(t, "rl_unexplainable", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "provider down",
			Err: &modelstep.TurnHostFailure{Code: "provider_unavailable", Message: "The model provider is unavailable."}}))
		_, run := f.StartChat("hello")
		exit, failed := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFailed || exit.Err != nil || exit.Steps != 1 {
			t.Fatalf("exit = %s after %d steps (%v), want failed after 1", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(failed, models.AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Explained || failed.FailureExplanation.Code != "provider_unavailable" {
			t.Fatalf("failure = %#v, want unexplained provider_unavailable", failed.FailureExplanation)
		}
		if got := len(f.Model.Calls("")); got != 1 {
			t.Fatalf("model calls = %d, want 1", got)
		}
		if failed.CreditError != nil {
			t.Fatalf("credit error = %#v on a provider outage", failed.CreditError)
		}
	})
}

func TestRunLoopCreditErrorFromFailureExplanation(t *testing.T) {
	looptest.EachStore(t, "rl_credit_error", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "no credits",
			Err: &modelstep.TurnHostFailure{Code: models.RunCreditErrorWorkspaceCreditsExhausted, Message: "The workspace has no credits left."}}))
		_, run := f.StartChat("hello")
		exit, failed := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitFailed || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want failed", exit.Reason, exit.Err)
		}
		f.MustStatus(failed, models.AgentRunStatusFailed)
		if failed.CreditError == nil || failed.CreditError.Code != models.RunCreditErrorWorkspaceCreditsExhausted {
			t.Fatalf("credit error = %#v, failure %#v", failed.CreditError, failed.FailureExplanation)
		}
		if got := len(f.Model.Calls("")); got != 1 {
			t.Fatalf("model calls = %d, want 1 (credits stop the explanation)", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Budget

// stepReservations counts the Run's step reservations (not the action
// reservations of governed calls, "action:<id>").
func stepReservations(run *models.AgentRun) int {
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
	looptest.EachStore(t, "rl_budget_steps", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(lookupRule("lookup", 0, purposeIs(models.ModelStepPurposeStep), nil)))
		run := f.StartBoundedRun("Check issues", models.BudgetPolicy{MaxTurns: 2})
		var reserved []string
		f.Dispatch.On("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			step := f.RunningStep(run.ID)
			current := f.MustRun(run.ID)
			if step == nil {
				t.Errorf("no running step during the lookup")
			} else if reservation, ok := current.BudgetReservations[step.ID]; !ok || reservation.Usage.Turns != 1 || stepReservations(current) != 1 {
				t.Errorf("reservations during step %d = %#v, want one step reservation, under the step id with Turns 1", step.Sequence, current.BudgetReservations)
			} else {
				reserved = append(reserved, step.ID)
			}
			return map[string]interface{}{"id": in.Arguments["id"], "status": "open"}, nil
		})
		exit, paused := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitPaused || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want paused after 2", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(paused, models.AgentRunStatusPaused)
		if paused.BudgetUsage.Turns != 2 || len(paused.BudgetReservations) != 0 || paused.BudgetState != models.BudgetStateExhausted {
			t.Fatalf("budget = usage %#v reservations %#v state %s", paused.BudgetUsage, paused.BudgetReservations, paused.BudgetState)
		}
		if len(reserved) != 2 {
			t.Fatalf("reserved steps = %v", reserved)
		}
		exhausted := f.Events(run.ID, "budget.exhausted")
		if len(exhausted) != 1 || exhausted[0].TurnID != reserved[1] {
			t.Fatalf("budget.exhausted = %#v, want one on the second step", exhausted)
		}
	})
}

func TestRunLoopBudgetAdmissionRefusesModelStep(t *testing.T) {
	looptest.EachStore(t, "rl_budget_admission", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(lookupRule("lookup", 0, nil, nil)))
		run := f.StartBoundedRun("Check issues", models.BudgetPolicy{MaxInputTokens: 10})
		exit, paused := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitPaused || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want paused", exit.Reason, exit.Err)
		}
		f.MustStatus(paused, models.AgentRunStatusPaused)
		steps := f.SortedSteps(run.ID)
		if len(steps) != 1 {
			t.Fatalf("steps = %d, want 1", len(steps))
		}
		step := steps[0]
		if step.Status != models.AgentTurnStatusCanceled || step.FailureCode != runloop.StepFailureBudgetExhausted || step.BudgetAdmission == nil || step.BudgetAdmission.TurnID != step.ID {
			t.Fatalf("step = %s %q admission %#v", step.Status, step.FailureCode, step.BudgetAdmission)
		}
		if got := len(f.Model.Calls("")); got != 0 {
			t.Fatalf("model calls = %d, want none", got)
		}
		if len(f.Events(run.ID, "budget.exhausted")) != 1 || paused.BudgetUsage.Turns != 0 {
			t.Fatalf("budget.exhausted = %d, turns %d", len(f.Events(run.ID, "budget.exhausted")), paused.BudgetUsage.Turns)
		}
	})
}

func TestRunLoopDurationBudgetCancelsStep(t *testing.T) {
	looptest.EachStore(t, "rl_budget_duration", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "slow",
			Before: func(ctx context.Context, _ looptest.StepView) error {
				<-ctx.Done()
				return ctx.Err()
			},
			Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Too late.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "1"}))
			}}))
		run := f.StartBoundedRun("Check issue 1", models.BudgetPolicy{MaxDurationMS: 200})
		exit, paused := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitPaused || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want paused", exit.Reason, exit.Err)
		}
		f.MustStatus(paused, models.AgentRunStatusPaused)
		steps := f.SortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != models.AgentTurnStatusCanceled || steps[0].FailureCode != runloop.StepFailureDurationBudget {
			t.Fatalf("steps = %#v", steps)
		}
		if got := strings.Join(f.EntryTypes(run.ID), ","); got != "user" {
			t.Fatalf("log = %s, want only the goal: nothing of the cut step is appended", got)
		}
		if f.Dispatch.Count("") != 0 {
			t.Fatal("a tool ran after the deadline")
		}
	})
}

// ---------------------------------------------------------------------------
// Cancel and lease loss

func TestRunLoopCanceledRunDiscardsOutput(t *testing.T) {
	looptest.EachStore(t, "rl_cancel", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		f = looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "canceled meanwhile",
			Before: func(ctx context.Context, v looptest.StepView) error {
				current := f.MustRun(v.Request().RunID)
				_, err := agentrun.NewRunCommandService(f.Chats).CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
					Scope: f.Scope, RunID: current.ID, ExpectedRevision: current.Revision, Kind: agentrun.AgentRunCommandCancel,
					Actor: models.ActivityActor{Type: "user", ID: looptest.FixtureUser}, Summary: "Stop",
				})
				return err
			},
			Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "1"}))
			}}))
		run := f.StartRun(models.RunKindAgentWork, "Check issue 1")
		exit, canceled := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitEnded {
			t.Fatalf("exit = %s (%v), want ended", exit.Reason, exit.Err)
		}
		f.MustStatus(canceled, models.AgentRunStatusCanceled)
		steps := f.SortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != models.AgentTurnStatusCanceled || steps[0].FailureCode != agentrun.StepFailureRunEnded {
			t.Fatalf("steps = %#v", steps)
		}
		if assistants := f.EntriesOfType(run.ID, string(models.SessionEntryAssistant)); len(assistants) != 0 {
			t.Fatalf("assistant entries = %d, want the output discarded", len(assistants))
		}
		if f.Dispatch.Count("") != 0 {
			t.Fatal("a tool ran for a canceled Run")
		}
	})
}

func TestRunLoopLeaseLostWritesNothing(t *testing.T) {
	looptest.EachStore(t, "rl_lease_lost", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		var entriesAtSteal int
		var revisionAtSteal int64
		f = looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "stolen meanwhile",
			Before: func(ctx context.Context, v looptest.StepView) error {
				f.Clock.Advance(looptest.FixtureLease + time.Second)
				stolen, err := f.Raw.ClaimNextAgentRun(ctx, models.AgentRunClaim{
					Scope: f.Scope, WorkerID: looptest.FixtureWorker2, Now: f.Clock.Now(), LeaseDuration: looptest.FixtureLease, AgingInterval: time.Minute,
				})
				if err != nil {
					return err
				}
				if stolen == nil || stolen.ID != v.Request().RunID {
					t.Errorf("the second worker did not take the Run: %#v", stolen)
					return errors.New("steal failed")
				}
				entriesAtSteal = len(f.Entries(stolen.ID))
				revisionAtSteal = stolen.Revision
				return nil
			},
			Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "1"}))
			}}))
		run := f.StartRun(models.RunKindAgentWork, "Check issue 1")
		exit, after := f.RunClaim(run.ID)
		if exit.Reason != runloop.RunLoopExitLeaseLost {
			t.Fatalf("exit = %s (%v), want lease_lost", exit.Reason, exit.Err)
		}
		if after.Status != models.AgentRunStatusRunning || after.LeaseOwner != looptest.FixtureWorker2 || after.Revision != revisionAtSteal {
			t.Fatalf("run after = %s owner %s revision %d, want untouched since the steal (revision %d)",
				after.Status, after.LeaseOwner, after.Revision, revisionAtSteal)
		}
		if got := len(f.Entries(run.ID)); got != entriesAtSteal {
			t.Fatalf("session entries %d -> %d after the lease was lost", entriesAtSteal, got)
		}
		if f.Dispatch.Count("") != 0 {
			t.Fatal("a tool ran after the lease was lost")
		}
		for _, event := range f.Events(run.ID) {
			if event.Actor.ID == looptest.FixtureWorker1 && event.CreatedAt.After(f.Clock.Now().Add(-time.Second)) {
				t.Fatalf("worker 1 wrote %s after losing the lease", event.EventType)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Deterministic producers

func TestRunLoopDeterministicProducerKernelState(t *testing.T) {
	looptest.EachStore(t, "rl_deterministic", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store)
		producer := f.ScriptedRunbook(false)
		run := f.StartRun(models.RunKindAgentWork, "Run the issue runbook")
		exit, done := f.RunClaimWith(f.Ctx, run.ID, func(b *agentrun.TurnRunnerBinding) { b.Producer = producer })
		if exit.Reason != runloop.RunLoopExitFinal || exit.Err != nil || exit.Steps != 2 {
			t.Fatalf("exit = %s after %d steps (%v), want final after 2", exit.Reason, exit.Steps, exit.Err)
		}
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if len(f.Model.Calls("")) != 0 {
			t.Fatal("a deterministic Run called the model")
		}
		if producer.Result == nil || producer.Result.State != models.ToolEntryCompleted || producer.Result.Output["title"] != "Issue 5" {
			t.Fatalf("second step read %#v, want the lookup result", producer.Result)
		}
		if len(producer.Steps) != 2 || producer.Steps[1]["phase"] != "lookup" {
			t.Fatalf("engine state seen by the steps = %#v, want step 1's state before step 2", producer.Steps)
		}
		if done.Checkpoint["phase"] != "done" || done.Output["title"] != "Issue 5" {
			t.Fatalf("run checkpoint %#v output %#v", done.Checkpoint, done.Output)
		}
		steps := f.SortedSteps(run.ID)
		if len(steps) != 2 || steps[0].Producer != models.StepProducerDeterministic || steps[0].ToolCallCount != 1 {
			t.Fatalf("steps = %#v", steps)
		}
		if f.Dispatch.Count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.Dispatch.Count("lookup"))
		}
		if got := strings.Join(f.EntryTypes(run.ID), ","); !strings.HasSuffix(got, "tool:completed") || strings.Contains(got, "assistant") {
			t.Fatalf("log = %s, want tool entries only after the goal", got)
		}
	})
}

func TestRunLoopDeterministicParkUsesLoopTransition(t *testing.T) {
	looptest.EachStore(t, "rl_deterministic_park", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store)
		producer := f.ScriptedRunbook(true)
		run := f.StartRun(models.RunKindAgentWork, "Wait for the requester")
		exit, parked := f.RunClaimWith(f.Ctx, run.ID, func(b *agentrun.TurnRunnerBinding) { b.Producer = producer })
		if exit.Reason != runloop.RunLoopExitParked || exit.Err != nil {
			t.Fatalf("exit = %s (%v), want parked", exit.Reason, exit.Err)
		}
		f.MustStatus(parked, models.AgentRunStatusWaitingForEvent)
		steps := f.SortedSteps(run.ID)
		if len(steps) != 1 || steps[0].Status != models.AgentTurnStatusCompleted {
			t.Fatalf("steps = %#v", steps)
		}
		if transitions := f.StepTransitions(run.ID, steps[0].ID); len(transitions) != 1 {
			t.Fatalf("transitions applying the step = %d, want 1", len(transitions))
		}
		if parked.Checkpoint["phase"] != "waiting" || parked.WakeCondition == nil || parked.WakeCondition.Type != "user_message" || parked.LastAppliedTurn != 1 {
			t.Fatalf("parked run checkpoint %#v wake %#v applied %d", parked.Checkpoint, parked.WakeCondition, parked.LastAppliedTurn)
		}
	})
}
