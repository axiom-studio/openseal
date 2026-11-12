package runtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

type foregroundClarificationFixture struct {
	store        conversationTaskClarificationStore
	service      *conversation.ConversationService
	scheduler    *conversationrun.ConversationRunScheduler
	conversation *Conversation
	trigger      *ChannelMessage
	run          *AgentRun
	turn         *AgentTurn
	asked        string
}

func newForegroundClarificationFixture(t *testing.T, store conversationTaskClarificationStore, threaded bool) *foregroundClarificationFixture {
	t.Helper()
	f := &foregroundClarificationFixture{store: store, service: conversation.NewConversationService(store)}
	var err error
	seedTestSpace(t, store.(SpaceStore), Scope{Kind: "organization", ID: "foreground-clarification"})
	f.conversation, _, err = f.service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: Scope{Kind: "organization", ID: "foreground-clarification"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "browser-agent"},
		Title: "Browser research", IdempotencyKey: "browser-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler, err = conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.trigger = f.post(t, conversation.PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"},
		Intent: MessageIntentQuestion, Content: "Research the requested page.", RequiresResponse: true, StartThread: threaded, IdempotencyKey: "request"})
	result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.conversation.Scope, f.conversation.ID, f.trigger.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", result, err)
	}
	f.run = result.Run
	return f
}

func (f *foregroundClarificationFixture) post(t *testing.T, req conversation.PostChannelMessageRequest) *ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Scope, req.ConversationID, req.ExpectedRevision = current.Scope, current.ID, current.Revision
	req.Audience = ConversationAudience{Kind: ConversationAudienceChannel}
	result, err := f.service.PostChannelMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

// ask parks the foreground Run on an ask_user call (wakeType user_message),
// or on another event wait that is not a question.
func (f *foregroundClarificationFixture) ask(t *testing.T, question, wakeType string) {
	t.Helper()
	claimed := claimAskUserRun(t, f.store, f.run, "foreground-worker")
	before, _ := f.store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if wakeType == "user_message" {
		parked, call := askUserParked(t, f.store, claimed, "foreground-worker", question)
		f.run, f.turn, f.asked = parked, call.step, question
		return
	}
	parked, _, err := NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), claimed.Scope, claimed.ID, RunTransitionRequest{
		ExpectedRevision: claimed.Revision, Status: AgentRunStatusWaitingForEvent, LeaseOwner: "foreground-worker",
		WakeCondition: &WakeCondition{Type: wakeType, Reference: f.conversation.ID}, AppliedTurn: claimed.LastAppliedTurn + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.run, f.turn, f.asked = parked, nil, question
	after, _ := f.store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if len(before) != len(after) {
		t.Fatal("a non-question wait posted a message")
	}
}

// parkedQuestionKey is the idempotency key of the question run is parked on,
// "" when it is not parked.
func parkedQuestionKey(run *AgentRun) string {
	if run == nil || run.WakeCondition == nil {
		return ""
	}
	return conversation.ConversationClarificationKey(run.ID, run.WakeCondition.Reference)
}

func (f *foregroundClarificationFixture) question(t *testing.T) *ChannelMessage {
	t.Helper()
	if _, err := f.scheduler.ReconcileScope(t.Context(), f.run.Scope); err != nil {
		t.Fatal(err)
	}
	question, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, parkedQuestionKey(f.run))
	if err != nil || question == nil || question.Intent != MessageIntentQuestion || !question.RequiresResponse || question.ReplyToMessageID != f.trigger.ID ||
		question.Content != f.asked || len(question.References) != 1 || question.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: f.run.ID}) {
		t.Fatalf("foreground question = %#v %v", question, err)
	}
	return question
}

func (f *foregroundClarificationFixture) current(t *testing.T) *AgentRun {
	t.Helper()
	run, err := f.store.GetAgentRun(t.Context(), f.run.Scope, f.run.ID)
	if err != nil || run == nil {
		t.Fatalf("read foreground: %#v %v", run, err)
	}
	return run
}

func (f *foregroundClarificationFixture) assertOnlyRun(t *testing.T) {
	t.Helper()
	runs, err := f.store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: f.run.Scope, Limit: 100})
	if err != nil || len(runs) != 1 || runs[0].ID != f.run.ID || runs[0].Context[ConversationTaskContextKey] != nil {
		t.Fatalf("clarification forked or promoted foreground work: %#v %v", runs, err)
	}
}

func TestForegroundConversationClarificationResumesSameRun(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		name := "implicit"
		if threaded {
			name = "explicit thread"
		}
		t.Run(name, func(t *testing.T) {
			forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
				f := newForegroundClarificationFixture(t, store, threaded)
				f.ask(t, "The page blocked this browser. Would you like to try the alternative browser?", "user_message")
				question := f.question(t)
				if again := f.question(t); again.ID != question.ID {
					t.Fatal("foreground question duplicated")
				}
				req := conversation.PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer, Content: "Yes, try it for this page.", RequiresResponse: true, IdempotencyKey: "answer"}
				if threaded {
					req.ReplyToMessageID, req.ResolvesMessageID = question.ID, question.ID
				}
				answer := f.post(t, req)
				// Recovery may replay the original prompt before scheduling the answer.
				if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, f.trigger.ID); err != nil {
					t.Fatal(err)
				}
				if f.current(t).Status != AgentRunStatusWaitingForEvent {
					t.Fatal("pending answer superseded its own foreground request")
				}
				resumed, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
				if err != nil || replayed || resumed == nil || resumed.Run.ID != f.run.ID || resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil {
					t.Fatalf("foreground resume = %#v %v %v", resumed, replayed, err)
				}
				f.run = resumed.Run
				want := &ConversationAnswerReceipt{Scope: f.run.Scope, RunID: f.run.ID, AgentID: f.run.AssignedAgentID, ConversationID: f.conversation.ID,
					SourceRunID: f.run.ID, SourceMessageID: f.trigger.ID, ThreadRootMessageID: question.ThreadRootID,
					QuestionMessageID: question.ID, QuestionTurnID: f.turn.ID, QuestionTurnSequence: f.turn.Sequence, QuestionContent: question.Content,
					AnswerMessageID: answer.ID, AnswerContent: answer.Content, AuthenticatedActor: f.trigger.Sender}
				if len(f.run.PendingInterventions) != 1 || !reflect.DeepEqual(f.run.PendingInterventions[0].ConversationAnswer, want) {
					t.Fatalf("foreground receipt = %#v; want %#v", f.run.PendingInterventions, want)
				}
				for _, id := range []string{f.trigger.ID, answer.ID} {
					if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, id); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.scheduler.ReconcileScope(t.Context(), f.run.Scope); err != nil {
					t.Fatal(err)
				}
				if f.current(t).Status != AgentRunStatusQueued || len(f.current(t).PendingInterventions) != 1 {
					t.Fatal("replay canceled or duplicated accepted foreground guidance")
				}
				activity := NewRunActivityService(store, store)
				running, _, err := activity.TransitionRun(t.Context(), f.run.Scope, f.run.ID, RunTransitionRequest{ExpectedRevision: f.run.Revision, Status: AgentRunStatusRunning})
				if err != nil {
					t.Fatal(err)
				}
				finished, _, err := activity.TransitionRun(t.Context(), f.run.Scope, f.run.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted})
				if err != nil {
					t.Fatal(err)
				}
				again, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
				if err != nil || !replayed || again == nil || again.Run.ID != finished.ID || again.Run.Status != AgentRunStatusCompleted || len(again.Run.PendingInterventions) != 1 {
					t.Fatalf("completed foreground answer replay = %#v %v %v", again, replayed, err)
				}
				f.assertOnlyRun(t)
			})
		})
	}
}

func TestForegroundConversationClarificationRejectsForeignAndInactiveAnswers(t *testing.T) {
	for _, reason := range []string{"other actor", "other thread", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
				f := newForegroundClarificationFixture(t, store, true)
				f.ask(t, "Use the alternative browser for this page?", "user_message")
				question := f.question(t)
				req := conversation.PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer, Content: "Yes", RequiresResponse: true,
					ReplyToMessageID: question.ID, ResolvesMessageID: question.ID, IdempotencyKey: "rejected-answer"}
				switch reason {
				case "other actor":
					req.Sender.ID = "other-member"
				case "other thread":
					other := f.post(t, conversation.PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentUpdate, Content: "Another thread", StartThread: true, IdempotencyKey: "other-thread"})
					req.ReplyToMessageID = other.ID
				case "canceled":
					result, err := agentrun.NewRunCommandService(store).CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: f.run.Scope, RunID: f.run.ID,
						ExpectedRevision: f.run.Revision, Kind: agentrun.AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: f.trigger.Sender.ID}})
					if err != nil {
						t.Fatal(err)
					}
					f.run = result.Run
				}
				answer := f.post(t, req)
				before, _ := json.Marshal(f.current(t))
				for range 2 {
					result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
					if err != nil || result != nil {
						t.Fatalf("rejected answer scheduled work: %#v %v", result, err)
					}
				}
				after, _ := json.Marshal(f.current(t))
				if string(before) != string(after) {
					t.Fatal("rejected answer changed foreground state")
				}
				f.assertOnlyRun(t)
			})
		})
	}
}

func TestForegroundConversationClarificationReplayKeepsLaterQuestionWaiting(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newForegroundClarificationFixture(t, store, true)
		f.ask(t, "Use the alternative browser?", "user_message")
		question := f.question(t)
		answer := f.post(t, conversation.PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer, Content: "Yes", RequiresResponse: true,
			ReplyToMessageID: question.ID, ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
		accepted, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
		if err != nil || accepted == nil {
			t.Fatal(err)
		}
		f.run = accepted.Run
		firstReceipt := *f.run.PendingInterventions[0].ConversationAnswer
		f.ask(t, "The next site also requires a different browser. Try it there?", "user_message")
		second := f.question(t)
		if second.ID == question.ID || f.turn.ID == firstReceipt.QuestionTurnID {
			t.Fatal("later question reused its predecessor's identity")
		}
		for _, id := range []string{f.trigger.ID, answer.ID} {
			if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, id); err != nil {
				t.Fatal(err)
			}
		}
		current := f.current(t)
		if current.Status != AgentRunStatusWaitingForEvent || len(current.PendingInterventions) != 1 || !reflect.DeepEqual(current.PendingInterventions[0].ConversationAnswer, &firstReceipt) {
			t.Fatal("old affirmative answer resumed a later question")
		}
		f.assertOnlyRun(t)
	})
}

func TestForegroundConversationQuestionsRequireUserMessageWait(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newForegroundClarificationFixture(t, store, false)
		f.ask(t, "Waiting for the provider callback.", "webhook")
		if _, err := f.scheduler.ReconcileScope(t.Context(), f.run.Scope); err != nil {
			t.Fatal(err)
		}
		question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, parkedQuestionKey(f.run))
		if err != nil || question != nil {
			t.Fatalf("provider wait was projected as a human question: %#v %v", question, err)
		}
		f.assertOnlyRun(t)
	})
}
