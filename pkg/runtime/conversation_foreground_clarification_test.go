package runtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control/controltest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func TestForegroundConversationClarificationResumesSameRun(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		name := "implicit"
		if threaded {
			name = "explicit thread"
		}
		t.Run(name, func(t *testing.T) {
			controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
				f := controltest.NewForegroundClarification(t, store, threaded)
				f.Ask(t, "The page blocked this browser. Would you like to try the alternative browser?", "user_message")
				question := f.Question(t)
				if again := f.Question(t); again.ID != question.ID {
					t.Fatal("foreground question duplicated")
				}
				req := conversation.PostChannelMessageRequest{Sender: f.Trigger.Sender, Intent: MessageIntentAnswer, Content: "Yes, try it for this page.", RequiresResponse: true, IdempotencyKey: "answer"}
				if threaded {
					req.ReplyToMessageID, req.ResolvesMessageID = question.ID, question.ID
				}
				answer := f.Post(t, req)
				// Recovery may replay the original prompt before scheduling the answer.
				if _, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, f.Trigger.ID); err != nil {
					t.Fatal(err)
				}
				if f.Current(t).Status != AgentRunStatusWaitingForEvent {
					t.Fatal("pending answer superseded its own foreground request")
				}
				resumed, replayed, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, answer.ID)
				if err != nil || replayed || resumed == nil || resumed.Run.ID != f.Run.ID || resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil {
					t.Fatalf("foreground resume = %#v %v %v", resumed, replayed, err)
				}
				f.Run = resumed.Run
				want := &ConversationAnswerReceipt{Scope: f.Run.Scope, RunID: f.Run.ID, AgentID: f.Run.AssignedAgentID, ConversationID: f.Conversation.ID,
					SourceRunID: f.Run.ID, SourceMessageID: f.Trigger.ID, ThreadRootMessageID: question.ThreadRootID,
					QuestionMessageID: question.ID, QuestionTurnID: f.Turn.ID, QuestionTurnSequence: f.Turn.Sequence, QuestionContent: question.Content,
					AnswerMessageID: answer.ID, AnswerContent: answer.Content, AuthenticatedActor: f.Trigger.Sender}
				if len(f.Run.PendingInterventions) != 1 || !reflect.DeepEqual(f.Run.PendingInterventions[0].ConversationAnswer, want) {
					t.Fatalf("foreground receipt = %#v; want %#v", f.Run.PendingInterventions, want)
				}
				for _, id := range []string{f.Trigger.ID, answer.ID} {
					if _, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, id); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.Scheduler.ReconcileScope(t.Context(), f.Run.Scope); err != nil {
					t.Fatal(err)
				}
				if f.Current(t).Status != AgentRunStatusQueued || len(f.Current(t).PendingInterventions) != 1 {
					t.Fatal("replay canceled or duplicated accepted foreground guidance")
				}
				activity := NewRunActivityService(store, store)
				running, _, err := activity.TransitionRun(t.Context(), f.Run.Scope, f.Run.ID, RunTransitionRequest{ExpectedRevision: f.Run.Revision, Status: AgentRunStatusRunning})
				if err != nil {
					t.Fatal(err)
				}
				finished, _, err := activity.TransitionRun(t.Context(), f.Run.Scope, f.Run.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted})
				if err != nil {
					t.Fatal(err)
				}
				again, replayed, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, answer.ID)
				if err != nil || !replayed || again == nil || again.Run.ID != finished.ID || again.Run.Status != AgentRunStatusCompleted || len(again.Run.PendingInterventions) != 1 {
					t.Fatalf("completed foreground answer replay = %#v %v %v", again, replayed, err)
				}
				f.AssertOnlyRun(t)
			})
		})
	}
}

func TestForegroundConversationClarificationRejectsForeignAndInactiveAnswers(t *testing.T) {
	for _, reason := range []string{"other actor", "other thread", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
				f := controltest.NewForegroundClarification(t, store, true)
				f.Ask(t, "Use the alternative browser for this page?", "user_message")
				question := f.Question(t)
				req := conversation.PostChannelMessageRequest{Sender: f.Trigger.Sender, Intent: MessageIntentAnswer, Content: "Yes", RequiresResponse: true,
					ReplyToMessageID: question.ID, ResolvesMessageID: question.ID, IdempotencyKey: "rejected-answer"}
				switch reason {
				case "other actor":
					req.Sender.ID = "other-member"
				case "other thread":
					other := f.Post(t, conversation.PostChannelMessageRequest{Sender: f.Trigger.Sender, Intent: MessageIntentUpdate, Content: "Another thread", StartThread: true, IdempotencyKey: "other-thread"})
					req.ReplyToMessageID = other.ID
				case "canceled":
					result, err := agentrun.NewRunCommandService(store).CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: f.Run.Scope, RunID: f.Run.ID,
						ExpectedRevision: f.Run.Revision, Kind: agentrun.AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: f.Trigger.Sender.ID}})
					if err != nil {
						t.Fatal(err)
					}
					f.Run = result.Run
				}
				answer := f.Post(t, req)
				before, _ := json.Marshal(f.Current(t))
				for range 2 {
					result, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, answer.ID)
					if err != nil || result != nil {
						t.Fatalf("rejected answer scheduled work: %#v %v", result, err)
					}
				}
				after, _ := json.Marshal(f.Current(t))
				if string(before) != string(after) {
					t.Fatal("rejected answer changed foreground state")
				}
				f.AssertOnlyRun(t)
			})
		})
	}
}

func TestForegroundConversationClarificationReplayKeepsLaterQuestionWaiting(t *testing.T) {
	controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
		f := controltest.NewForegroundClarification(t, store, true)
		f.Ask(t, "Use the alternative browser?", "user_message")
		question := f.Question(t)
		answer := f.Post(t, conversation.PostChannelMessageRequest{Sender: f.Trigger.Sender, Intent: MessageIntentAnswer, Content: "Yes", RequiresResponse: true,
			ReplyToMessageID: question.ID, ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
		accepted, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, answer.ID)
		if err != nil || accepted == nil {
			t.Fatal(err)
		}
		f.Run = accepted.Run
		firstReceipt := *f.Run.PendingInterventions[0].ConversationAnswer
		f.Ask(t, "The next site also requires a different browser. Try it there?", "user_message")
		second := f.Question(t)
		if second.ID == question.ID || f.Turn.ID == firstReceipt.QuestionTurnID {
			t.Fatal("later question reused its predecessor's identity")
		}
		for _, id := range []string{f.Trigger.ID, answer.ID} {
			if _, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, id); err != nil {
				t.Fatal(err)
			}
		}
		current := f.Current(t)
		if current.Status != AgentRunStatusWaitingForEvent || len(current.PendingInterventions) != 1 || !reflect.DeepEqual(current.PendingInterventions[0].ConversationAnswer, &firstReceipt) {
			t.Fatal("old affirmative answer resumed a later question")
		}
		f.AssertOnlyRun(t)
	})
}

func TestForegroundConversationQuestionsRequireUserMessageWait(t *testing.T) {
	controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
		f := controltest.NewForegroundClarification(t, store, false)
		f.Ask(t, "Waiting for the provider callback.", "webhook")
		if _, err := f.Scheduler.ReconcileScope(t.Context(), f.Run.Scope); err != nil {
			t.Fatal(err)
		}
		question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.Run.Scope, f.Conversation.ID, controltest.ParkedQuestionKey(f.Run))
		if err != nil || question != nil {
			t.Fatalf("provider wait was projected as a human question: %#v %v", question, err)
		}
		f.AssertOnlyRun(t)
	})
}
