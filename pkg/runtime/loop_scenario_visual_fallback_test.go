package runtime

// Run-loop scenarios for an answer whose visual blocks are wrong: the answer
// has already streamed to the person, so a failing block is asked for again
// on its own, once, and then goes without it; the Run records why. A visual
// never costs the answer.

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
)

// blocksAnswer is lead followed by each component of visual as a block,
// the way the model writes an answer with visuals.
func blocksAnswer(lead string, visual *presentation.Visual) string {
	parts := []string{lead}
	for _, component := range visual.Components {
		parts = append(parts, presentation.FormatBlock(component))
	}
	return strings.Join(parts, "\n\n")
}

// answerSteps replies with the step's entry of replies, the last one again
// once they run out.
func answerSteps(replies ...string) *looptest.ModelRule {
	return &looptest.ModelRule{Name: "answer", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
		index := min(v.Step(), len(replies)) - 1
		return looptest.Say(replies[index])
	}}
}

// A block that fails is asked for again once, on its own: the next reply
// holds only the corrected block. A repair that still fails costs only that
// block, never the answer.
func TestLoopFailingBlockIsRepairedOnceThenLeftOut(t *testing.T) {
	broken := ":::hologram{#h}\nCheaper in spring.\n:::"
	cases := map[string]struct {
		repair  string
		posted  string
		dropped bool
	}{
		"block_repaired": {repair: ":::callout{#h}\nbody: Cheaper in spring.\n:::", posted: "Spring is cheapest.\n\n::visual{#h}"},
		"block_left_out": {repair: broken, posted: "Spring is cheapest.", dropped: true},
	}
	for name, tc := range cases {
		looptest.EachStore(t, name, func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(answerSteps("Spring is cheapest.\n\n"+broken, tc.repair)))
			content := &looptest.AnswerVisualContent{}
			convRunner(f, conversationrun.TurnRunnerConfig{AttachmentContent: content})
			conversation, run := f.StartChat("when is it cheapest?")
			done := f.DriveToRest(run.ID, 4)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			rejections := portFinalRejections(f, run.ID)
			if finals := portFinalReplies(f, run.ID); len(finals) != 2 || len(rejections) != 1 || rejections[0].StepID != finals[0].StepID ||
				!strings.Contains(rejections[0].Note.Text, "Reply with only the corrected blocks") {
				t.Fatalf("final replies = %v", f.EntryTypes(run.ID))
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 {
				t.Fatalf("answers = %#v", answers)
			}
			events := f.Events(run.ID, "turn.visual_rejected")
			if tc.dropped {
				if !portVisualNotShown(answers[0].Content, tc.posted) || len(answers[0].References) != 1 || content.Puts != 0 || len(events) != 1 {
					t.Fatalf("answer %q refs %d puts %d events %d", answers[0].Content, len(answers[0].References), content.Puts, len(events))
				}
				return
			}
			if answers[0].Content != tc.posted || len(answers[0].References) != 2 || content.Puts != 1 || len(events) != 0 {
				t.Fatalf("answer %q refs %d puts %d events %d", answers[0].Content, len(answers[0].References), content.Puts, len(events))
			}
		})
	}
}

// portVisualNotShown: the posted answer is the model's text followed by the
// line saying its visual could not be shown.
func portVisualNotShown(posted, answer string) bool {
	return strings.HasPrefix(posted, answer+"\n\n") && strings.Contains(posted, "couldn't be shown")
}
