package runloop

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/control/controltest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func TestAskUserAnswerResumesWithAnswerAndConsumesIntervention(t *testing.T) {
	controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
		f := controltest.ClaimedForegroundRun(t, store)
		parked, call := controltest.AskUserParked(t, store, f.Run, "foreground-worker", controltest.AskUserQuestion)
		f.Run = parked
		question := f.Question(t)

		// Not answered yet: the resume re-parks on the same entry.
		answer := f.Post(t, conversation.PostChannelMessageRequest{Sender: f.Trigger.Sender, Intent: models.MessageIntentAnswer,
			Content: "The pricing page.", ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
		resumed, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, answer.ID)
		if err != nil || resumed == nil || resumed.Run.ID != f.Run.ID || resumed.Run.Status != models.AgentRunStatusQueued {
			t.Fatalf("answer did not wake the Run: %#v %v", resumed, err)
		}
		receipt := resumed.Run.PendingInterventions[len(resumed.Run.PendingInterventions)-1].ConversationAnswer
		if receipt == nil || receipt.QuestionTurnID != call.Step.ID || receipt.QuestionTurnSequence != call.Step.Sequence ||
			receipt.QuestionMessageID != question.ID || receipt.AnswerMessageID != answer.ID {
			t.Fatalf("receipt not bound to the ask_user step: %#v", receipt)
		}
		result, claimed := call.Resume(t)
		if !result.Resolved || result.Result.Kind != control.ControlContinue || call.Entry.State != models.ToolEntryCompleted {
			t.Fatalf("resume = %#v", result)
		}
		value := controltest.AskUserResultValue(t, call.Entry)
		if value["status"] != "answered" || value["answer"] != "The pricing page." || value["messageId"] != answer.ID {
			t.Fatalf("result = %#v", value)
		}
		// The answer is the tool result; steering consumes it without a user entry.
		steering, err := consumeSteering(claimed, &models.AgentTurn{ID: "next", RunID: claimed.ID, Sequence: call.Step.Sequence + 1}, call.Session)
		if err != nil || len(steering.Entries) != 0 || steering.Through == nil || *steering.Through != len(claimed.PendingInterventions) {
			t.Fatalf("steering = %#v %v", steering, err)
		}

		// The same question again is answered from the session.
		again := controltest.NewAskUserCall(t, store, claimed, "foreground-worker", controltest.AskUserQuestion)
		refused := again.Settle(t)
		value = controltest.AskUserResultValue(t, refused.Entry)
		if refused.Kind != control.ControlContinue || refused.Entry.State != models.ToolEntryError || value["error"] != "already_asked" ||
			!strings.Contains(value["message"].(string), "The pricing page.") {
			t.Fatalf("repeat = %#v %#v", refused, value)
		}
	})
}
