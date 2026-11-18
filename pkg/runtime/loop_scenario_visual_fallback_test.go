package runtime

// Run-loop scenarios for an answer whose optional visual is wrong: the answer
// has already streamed to the person, so it goes without the visual and the
// Run records why. A visual never costs the answer.

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
)

// visualWithoutComponents fails Validate: there is nothing to draw.
func visualWithoutComponents() map[string]any {
	return map[string]any{"title": "Prices", "components": []any{}}
}

// visualWithUnknownTypes has no component of a known type, so nothing to draw.
func visualWithUnknownTypes() map[string]any {
	return map[string]any{"title": "Prices", "components": []any{
		map[string]any{"id": "h", "type": "hologram", "text": "Cheaper in spring."},
	}}
}

// answerSteps replies with the step's entry of replies, the last one again
// once they run out.
func answerSteps(replies ...map[string]any) *looptest.ModelRule {
	return &looptest.ModelRule{Name: "answer", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
		index := min(v.Step(), len(replies)) - 1
		return looptest.FinalAnswer(replies[index])
	}}
}

func TestLoopInvalidVisualDeliversTheAnswerWithoutIt(t *testing.T) {
	for name, visual := range map[string]map[string]any{"visual_empty": visualWithoutComponents(), "visual_unknown_types": visualWithUnknownTypes()} {
		looptest.EachStore(t, name, func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(answerSteps(
				map[string]any{"answer": "Spring is cheapest.", "visual": visual},
			)))
			content := &looptest.AnswerVisualContent{}
			convRunner(f, conversationrun.TurnRunnerConfig{AttachmentContent: content})
			conversation, run := f.StartChat("when is it cheapest?")
			done := f.DriveToRest(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			if finals := portFinalEntries(f, run.ID); len(finals) != 1 || finals[0].State == models.ToolEntryError {
				t.Fatalf("final entries = %v", f.EntryTypes(run.ID))
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || !portVisualNotShown(answers[0].Content, "Spring is cheapest.") || len(answers[0].References) != 1 || content.Puts != 0 {
				t.Fatalf("answers = %#v (%d visual puts), want the answer without the visual", answers, content.Puts)
			}
			events := f.Events(run.ID, "turn.visual_rejected")
			if len(events) != 1 || events[0].Severity != models.ActivitySeverityWarning {
				t.Fatalf("visual rejected events = %#v", events)
			}
		})
	}
}

// Three rejected answers still fail the Run, and the chat is told the answer
// could not be delivered rather than that execution failed.
func TestLoopAnswerRejectedThreeTimesSaysSo(t *testing.T) {
	looptest.EachStore(t, "answer_rejected_reply", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(answerSteps(map[string]any{"answer": 42})))
		convRunner(f, conversationrun.TurnRunnerConfig{})
		conversation, run := f.StartChat("how is revenue?")
		failed := f.DriveToRest(run.ID, 3)
		f.MustStatus(failed, models.AgentRunStatusFailed)
		if failed.FailureExplanation == nil || failed.FailureExplanation.Code != runtimeconversation.RunFailureCodeFinalAnswerRejected {
			t.Fatalf("failure = %#v", failed.FailureExplanation)
		}
		reply := portReportFailure(f, conversation.ID, failed)
		if !strings.HasPrefix(reply.Content, "I worked on this but couldn't deliver my answer") {
			t.Fatalf("reply = %q", reply.Content)
		}
	})
}

// portVisualNotShown: the posted answer is the model's text followed by the
// line saying its visual could not be shown.
func portVisualNotShown(posted, answer string) bool {
	return strings.HasPrefix(posted, answer+"\n\n") && strings.Contains(posted, "couldn't be shown")
}
