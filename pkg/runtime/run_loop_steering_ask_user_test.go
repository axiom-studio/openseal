package runtime

import (
	"strings"
	"testing"
)

func TestAskUserAnswerResumesWithAnswerAndConsumesIntervention(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		parked, call := askUserParked(t, store, f.run, "foreground-worker", askUserTestQuestion)
		f.run = parked
		question := f.question(t)

		// Not answered yet: the resume re-parks on the same entry.
		answer := f.post(t, PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer,
			Content: "The pricing page.", ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
		resumed, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
		if err != nil || resumed == nil || resumed.Run.ID != f.run.ID || resumed.Run.Status != AgentRunStatusQueued {
			t.Fatalf("answer did not wake the Run: %#v %v", resumed, err)
		}
		receipt := resumed.Run.PendingInterventions[len(resumed.Run.PendingInterventions)-1].ConversationAnswer
		if receipt == nil || receipt.QuestionTurnID != call.step.ID || receipt.QuestionTurnSequence != call.step.Sequence ||
			receipt.QuestionMessageID != question.ID || receipt.AnswerMessageID != answer.ID {
			t.Fatalf("receipt not bound to the ask_user step: %#v", receipt)
		}
		result, claimed := call.resume(t)
		if !result.Resolved || result.Result.Kind != ControlContinue || call.entry.State != ToolEntryCompleted {
			t.Fatalf("resume = %#v", result)
		}
		value := askUserResultValue(t, call.entry)
		if value["status"] != "answered" || value["answer"] != "The pricing page." || value["messageId"] != answer.ID {
			t.Fatalf("result = %#v", value)
		}
		// The answer is the tool result; steering consumes it without a user entry.
		steering, err := consumeSteering(claimed, &AgentTurn{ID: "next", RunID: claimed.ID, Sequence: call.step.Sequence + 1}, call.session)
		if err != nil || len(steering.Entries) != 0 || steering.Through == nil || *steering.Through != len(claimed.PendingInterventions) {
			t.Fatalf("steering = %#v %v", steering, err)
		}

		// The same question again is answered from the session.
		again := newAskUserCall(t, store, claimed, "foreground-worker", askUserTestQuestion)
		refused := again.settle(t)
		value = askUserResultValue(t, refused.Entry)
		if refused.Kind != ControlContinue || refused.Entry.State != ToolEntryError || value["error"] != "already_asked" ||
			!strings.Contains(value["message"].(string), "The pricing page.") {
			t.Fatalf("repeat = %#v %#v", refused, value)
		}
	})
}
