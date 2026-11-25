package runtime

// Run-loop scenarios for a model call the provider cuts off at its output
// cap. A cut-off response with text or complete tool calls is kept: the
// calls run, a note asks the model to resume, and an answer written across
// cut-off steps is posted whole. One with nothing usable is discarded with a
// note, and three of those in a row fail the Run.

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

func outputCutoff() error {
	return &modelstep.TurnHostFailure{
		Code:              modelstep.TurnHostOutputTruncated,
		Message:           "Model output was truncated at 32000 tokens by the gateway's per-call output allowance for this model.",
		OutputLimitTokens: 32_000,
		OutputTokens:      31_990,
	}
}

func cutoffRule(times int) *looptest.ModelRule {
	return &looptest.ModelRule{Name: "cut off", Times: times, When: stepPurpose, Err: outputCutoff()}
}

// outputCutoffNotes is the Run's output_cutoff notes in log order.
func outputCutoffNotes(f *looptest.Fixture, runID string) []*models.SessionEntry {
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		if entry.Type == models.SessionEntryNote && entry.Note != nil && entry.Note.Kind == models.NoteOutputCutoff {
			out = append(out, entry)
		}
	}
	return out
}

// TestLoopOutputCutoffContinuesWithANote: the first step is cut off, the
// next one reads the note and answers, so the Run completes.
func TestLoopOutputCutoffContinuesWithANote(t *testing.T) {
	looptest.EachStore(t, "output_cutoff_continue", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			cutoffRule(1),
			&looptest.ModelRule{Name: "answer", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
				last := v.LastEntry()
				if last == nil || last.Note == nil || last.Note.Kind != string(models.NoteOutputCutoff) {
					t.Errorf("the step after the cutoff saw %s, want the cutoff note last", v.Describe())
				}
				return looptest.Say("Here is the short version; the full report is in report.md.")
			}},
		))
		conversation, run := f.StartChat("write the full research report")
		done := f.DriveToRest(run.ID, 2)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		notes := outputCutoffNotes(f, run.ID)
		if len(notes) != 1 {
			t.Fatalf("output_cutoff notes = %d, want 1", len(notes))
		}
		text := notes[0].Note.Text
		for _, want := range []string{"about 31990 tokens", "Nothing of that response was kept", "smaller pieces", "in parts"} {
			if !strings.Contains(text, want) {
				t.Fatalf("note %q does not say %q", text, want)
			}
		}
		steps := f.SortedSteps(run.ID)
		if len(steps) != 2 || steps[0].Status != models.AgentTurnStatusFailed || steps[0].FailureCode != modelstep.TurnHostOutputTruncated ||
			steps[1].Status != models.AgentTurnStatusCompleted {
			t.Fatalf("steps = %d, want a cut-off step then a completed one", len(steps))
		}
		if notes[0].StepID != steps[0].ID {
			t.Fatalf("note step = %s, want the cut-off step %s", notes[0].StepID, steps[0].ID)
		}
		if events := f.Events(run.ID, "run.output_cutoff"); len(events) != 1 || events[0].Severity != models.ActivitySeverityWarning {
			t.Fatalf("run.output_cutoff events = %d, want one warning", len(events))
		}
		answers := portAgentMessages(f, conversation.ID)
		if len(answers) != 1 || !strings.Contains(answers[0].Content, "report.md") {
			t.Fatalf("channel = %s, want the answer", looptest.DescribeMessages(f.Messages(conversation.ID)))
		}
	})
}

// TestLoopThirdConsecutiveOutputCutoffFailsTheRun: three cutoffs in a row
// fail the Run with provider_output_truncated and its existing reply.
func TestLoopThirdConsecutiveOutputCutoffFailsTheRun(t *testing.T) {
	looptest.EachStore(t, "output_cutoff_fail", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "cut off", Err: outputCutoff()}))
		conversation, run := f.StartChat("write the full research report")
		failed := f.DriveToRest(run.ID, 3)
		f.MustStatus(failed, models.AgentRunStatusFailed)
		portRequireTypedFailure(t, failed)
		if failed.FailureExplanation.Code != modelstep.TurnHostOutputTruncated {
			t.Fatalf("failure = %#v, want %s", failed.FailureExplanation, modelstep.TurnHostOutputTruncated)
		}
		if got := len(f.Model.Calls(models.ModelStepPurposeStep)); got != 3 {
			t.Fatalf("step calls = %d, want 3", got)
		}
		if notes := outputCutoffNotes(f, run.ID); len(notes) != 2 {
			t.Fatalf("output_cutoff notes = %d, want 2 (the third cutoff fails instead)", len(notes))
		}
		if reply := portReportFailure(f, conversation.ID, failed); reply.Content != runtimeconversation.TerminalRunFailureReply(f.Ctx, f.Raw, f.Raw, failed) ||
			!strings.Contains(reply.Content, "stopped before completing its response") {
			t.Fatalf("reply = %q, want the provider_output_truncated reply", reply.Content)
		}
	})
}

// TestLoopSuccessfulStepResetsOutputCutoffs: cutoff, cutoff, a completed
// step, cutoff, then an answer. The completed step resets the count, so the
// third cutoff does not fail the Run.
func TestLoopSuccessfulStepResetsOutputCutoffs(t *testing.T) {
	looptest.EachStore(t, "output_cutoff_reset", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			cutoffRule(2),
			lookupRule("lookup", 1, stepPurpose, nil),
			cutoffRule(1),
			&looptest.ModelRule{Name: "answer", When: stepPurpose, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Say("Issue 3 is open.")
			}},
		))
		_, run := f.StartChat("check issue 3")
		done := f.DriveToRest(run.ID, 2)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if notes := outputCutoffNotes(f, run.ID); len(notes) != 3 {
			t.Fatalf("output_cutoff notes = %d, want 3", len(notes))
		}
		var codes []string
		for _, step := range f.SortedSteps(run.ID) {
			codes = append(codes, string(step.Status)+":"+step.FailureCode)
		}
		want := "failed:provider_output_truncated,failed:provider_output_truncated,completed:,failed:provider_output_truncated,completed:"
		if got := strings.Join(codes, ","); got != want {
			t.Fatalf("steps = %s, want %s", got, want)
		}
		if f.Dispatch.Count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.Dispatch.Count("lookup"))
		}
	})
}

// cutOffReply is reply as a response the provider cut off at its output
// limit after outputTokens.
func cutOffReply(reply models.ModelStepResult, outputTokens int64) models.ModelStepResult {
	reply.Assistant.StopReason = models.ModelStopMaxTokens
	reply.Assistant.Usage.OutputTokens = outputTokens
	return reply
}

// TestLoopCutOffAnswerIsKeptAndResumed: an answer longer than one response
// is cut off twice; each part is kept, the model resumes after a note, and
// the posted answer is all three parts in order.
func TestLoopCutOffAnswerIsKeptAndResumed(t *testing.T) {
	looptest.EachStore(t, "output_cutoff_kept_text", func(t *testing.T, store models.KernelStore) {
		parts := []string{"The periodic table has 118 elem", "ents in 18 groups and 7 per", "iods."}
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "answer", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
			step := v.Step()
			if step > 1 {
				if last := v.LastEntry(); last == nil || last.Note == nil || last.Note.Kind != string(models.NoteOutputCutoff) {
					t.Errorf("step %d saw %s, want the cutoff note last", step, v.Describe())
				}
			}
			if step < len(parts) {
				return cutOffReply(looptest.Say(parts[step-1]), 8_000)
			}
			return looptest.Say(parts[len(parts)-1])
		}}))
		conversation, run := f.StartChat("explain the periodic table")
		done := f.DriveToRest(run.ID, 2)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		answers := portAgentMessages(f, conversation.ID)
		if len(answers) != 1 || answers[0].Content != strings.Join(parts, "") {
			t.Fatalf("channel = %s, want the whole answer once", looptest.DescribeMessages(f.Messages(conversation.ID)))
		}
		notes := outputCutoffNotes(f, run.ID)
		if len(notes) != 2 {
			t.Fatalf("output_cutoff notes = %d, want 2", len(notes))
		}
		for _, want := range []string{"about 8000 tokens", "was kept", "Resume directly", "joined to it"} {
			if !strings.Contains(notes[0].Note.Text, want) {
				t.Fatalf("note %q does not say %q", notes[0].Note.Text, want)
			}
		}
		var stops []string
		for _, step := range f.SortedSteps(run.ID) {
			stops = append(stops, string(step.Status)+":"+step.StopReason)
		}
		if got := strings.Join(stops, ","); got != "completed:max_tokens,completed:max_tokens,completed:end" {
			t.Fatalf("steps = %s", got)
		}
	})
}

// TestLoopCutOffToolCallsRun: a response cut off after a complete tool call
// keeps the call, which runs; the note follows its result.
func TestLoopCutOffToolCallsRun(t *testing.T) {
	looptest.EachStore(t, "output_cutoff_kept_calls", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "cut lookup", Times: 1, When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
				return cutOffReply(looptest.Act("Looking up issue 3.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "3"})), 12_000)
			}},
			&looptest.ModelRule{Name: "answer", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
				if last := v.LastEntry(); last == nil || last.Note == nil || last.Note.Kind != string(models.NoteOutputCutoff) {
					t.Errorf("the step after the cutoff saw %s, want the cutoff note last", v.Describe())
				}
				if len(v.ToolResultsSinceUser()) != 1 {
					t.Errorf("the step after the cutoff saw %s, want the lookup result", v.Describe())
				}
				return looptest.Say("Issue 3 is open.")
			}},
		))
		conversation, run := f.StartChat("check issue 3")
		done := f.DriveToRest(run.ID, 2)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if f.Dispatch.Count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.Dispatch.Count("lookup"))
		}
		notes := outputCutoffNotes(f, run.ID)
		if len(notes) != 1 || !strings.Contains(notes[0].Note.Text, "complete tool calls ran") {
			t.Fatalf("output_cutoff notes = %#v", notes)
		}
		if answers := portAgentMessages(f, conversation.ID); len(answers) != 1 || answers[0].Content != "Issue 3 is open." {
			t.Fatalf("channel = %s", looptest.DescribeMessages(f.Messages(conversation.ID)))
		}
	})
}

// TestLoopEndlessCutOffsFailTheRun: a model whose every response is cut off
// keeps going only up to the bound, then the Run fails as a cutoff.
func TestLoopEndlessCutOffsFailTheRun(t *testing.T) {
	looptest.EachStore(t, "output_cutoff_endless", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "rambling", When: stepPurpose, Reply: func(looptest.StepView) models.ModelStepResult {
			return cutOffReply(looptest.Say("and more "), 8_000)
		}}))
		_, run := f.StartChat("tell me everything")
		failed := f.DriveToRest(run.ID, 4)
		f.MustStatus(failed, models.AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != modelstep.TurnHostOutputTruncated {
			t.Fatalf("failure = %#v", failed.FailureExplanation)
		}
		if got := len(f.Model.Calls(models.ModelStepPurposeStep)); got != 12 {
			t.Fatalf("step calls = %d, want 12", got)
		}
	})
}
