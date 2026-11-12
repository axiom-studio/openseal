package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
)

func TestAskUserPostsQuestionAndParks(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		call := newAskUserCall(t, store, f.run, "foreground-worker", "  "+askUserTestQuestion+"  ")
		result := call.settle(t)
		if result.Kind != control.ControlParked || result.Park.Status != models.AgentRunStatusWaitingForEvent ||
			result.Park.Wake.Type != "user_message" || result.Park.Wake.Reference != call.entry.ID {
			t.Fatalf("park = %#v", result)
		}
		if call.entry.State != models.ToolEntryAwaitingEvent || call.entry.Tool.Wait == nil || call.entry.Tool.Wait.EventWaitID != call.entry.ID {
			t.Fatalf("entry = %#v", call.entry)
		}
		key := "conversation-clarification:" + f.run.ID + ":" + call.entry.ID
		meta, err := control.DecodeControlEntryMeta(call.entry)
		if err != nil || meta == nil || meta.Kind != models.ControlToolAskUser || meta.QuestionMessageID != models.StableConversationID(f.run.Scope, key, "message") {
			t.Fatalf("meta = %#v %v", meta, err)
		}
		question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, key)
		if err != nil || question == nil || question.ID != meta.QuestionMessageID || question.Content != askUserTestQuestion ||
			question.Intent != models.MessageIntentQuestion || !question.RequiresResponse || question.ReplyToMessageID != f.trigger.ID ||
			question.Sender != (models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: f.run.AssignedAgentID}) ||
			len(question.References) != 1 || question.References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: f.run.ID}) {
			t.Fatalf("question = %#v %v", question, err)
		}
		parked := call.park(t, result.Park)
		if parked.Status != models.AgentRunStatusWaitingForEvent || parked.WakeCondition == nil || parked.WakeCondition.Type != "user_message" ||
			parked.WakeCondition.Reference != call.entry.ID || parkedQuestionKey(parked) != key {
			t.Fatalf("parked run is not a question wait: %#v", parked)
		}
		// The reconciler replays the same message, never a second one.
		if _, err := f.scheduler.ReconcileScope(t.Context(), f.run.Scope); err != nil {
			t.Fatal(err)
		}
		messages, _ := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID, Limit: 100})
		if len(messages) != 2 {
			t.Fatalf("messages = %d, want trigger + question", len(messages))
		}
	})
}

func TestAskUserResumeWithoutAnswerRepostsAndReparks(t *testing.T) {
	store := conversationtest.NewStore(t)
	f := claimedForegroundRun(t, store)
	call := newAskUserCall(t, store, f.run, "foreground-worker", askUserTestQuestion)
	// Crash after the entry transition, before the post and the park: the
	// next claim resumes the awaiting entry.
	cc := call.context()
	cc.Arguments = &models.AskUserArgs{Question: askUserTestQuestion}
	entry, err := call.exec.TransitionControlEntry(t.Context(), cc, models.ToolEntryAwaitingEvent, &models.ToolEntryWait{EventWaitID: call.entry.ID, Reason: control.AskUserWaitReason}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call.entry = entry
	resumed, err := call.exec.Resume(t.Context(), call.context())
	if err != nil || resumed.Resolved || resumed.Park == nil || resumed.Park.Wake.Reference != entry.ID {
		t.Fatalf("resume = %#v %v", resumed, err)
	}
	key := conversation.ConversationClarificationKey(f.run.ID, entry.ID)
	question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, key)
	if err != nil || question == nil || question.Content != askUserTestQuestion {
		t.Fatalf("question not posted on resume: %#v %v", question, err)
	}
}

// conflictingQuestionStore makes every question post lose the conversation
// revision race.
type conflictingQuestionStore struct {
	models.ConversationStore
}

func (s conflictingQuestionStore) CommitChannelMessage(ctx context.Context, record models.ChannelMessageCommitRecord) (*models.ChannelMessageCommitResult, error) {
	if strings.HasPrefix(record.Message.IdempotencyKey, "conversation-clarification:") {
		return nil, models.ErrRevisionConflict
	}
	return s.ConversationStore.CommitChannelMessage(ctx, record)
}

func TestAskUserQuestionReconcilerRepostsFromEntry(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		call := newAskUserCall(t, store, f.run, "foreground-worker", askUserTestQuestion)
		call.exec.Conversations = conversation.NewConversationService(conflictingQuestionStore{store})
		result := call.settle(t)
		if result.Kind != control.ControlParked {
			t.Fatalf("settle = %#v", result)
		}
		key := conversation.ConversationClarificationKey(f.run.ID, call.entry.ID)
		if q, _ := store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, key); q != nil {
			t.Fatal("question posted despite conflicts")
		}
		f.run = call.park(t, result.Park)
		if f.run.Output["summary"] != nil {
			t.Fatal("question text leaked into Run output")
		}
		question := f.question(t)
		if question.Content != askUserTestQuestion || question.ID != models.StableConversationID(f.run.Scope, key, "message") {
			t.Fatalf("reconciled question = %#v", question)
		}
	})
}

func TestAskUserAnswerFromOtherActorIgnored(t *testing.T) {
	store := conversationtest.NewStore(t)
	f := claimedForegroundRun(t, store)
	parked, call := askUserParked(t, store, f.run, "foreground-worker", askUserTestQuestion)
	f.run = parked
	question := f.question(t)
	foreign := f.post(t, conversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "someone-else"},
		Intent: models.MessageIntentAnswer, Content: "Use the admin page.", ResolvesMessageID: question.ID, IdempotencyKey: "foreign-answer"})
	// An explicit reply to the question is a clarification attempt, but a
	// foreign actor's answer wakes nothing and starts nothing.
	if result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, foreign.ID); err != nil || result != nil {
		t.Fatalf("foreign answer accepted: %#v %v", result, err)
	}
	current := f.current(t)
	if current.Status != models.AgentRunStatusWaitingForEvent || len(current.PendingInterventions) != 0 {
		t.Fatalf("foreign answer woke the Run: %#v", current)
	}
	if answer := control.AskUserAnswer(current, call.entry, question.ID); answer != nil {
		t.Fatalf("foreign answer bound to the entry: %#v", answer)
	}
}

const askUserOptionsArguments = `{"question":"` + askUserTestQuestion + `","allowMultiple":true,` +
	`"options":[{"label":"Pricing"},{"label":"Docs","description":"The developer docs"},{"label":"Blog"}]}`

func TestAskUserOptionsPostedAndAnsweredWithSelectedOptions(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		call := newAskUserCallWithArguments(t, store, f.run, "foreground-worker", json.RawMessage(askUserOptionsArguments))
		result := call.settle(t)
		if result.Kind != control.ControlParked {
			t.Fatalf("settle = %#v", result)
		}
		f.run = call.park(t, result.Park)
		// The reconciler replays the same request, options included.
		question := f.question(t)
		want := &models.ChannelQuestion{AllowMultiple: true, Options: []models.ChannelQuestionOption{
			{ID: "o1", Label: "Pricing"}, {ID: "o2", Label: "Docs", Description: "The developer docs"}, {ID: "o3", Label: "Blog"},
		}}
		if !reflect.DeepEqual(question.Question, want) || question.Content != askUserTestQuestion {
			t.Fatalf("question = %#v", question.Question)
		}

		answer := f.post(t, conversation.PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: models.MessageIntentAnswer,
			Content: "Blog, Pricing", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o3", "o1"}, IdempotencyKey: "answer"})
		if !reflect.DeepEqual(answer.SelectedOptionIDs, []string{"o3", "o1"}) {
			t.Fatalf("answer = %#v", answer)
		}
		resumed, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
		if err != nil || resumed == nil || resumed.Run.ID != f.run.ID {
			t.Fatalf("answer did not wake the Run: %#v %v", resumed, err)
		}
		receipt := resumed.Run.PendingInterventions[len(resumed.Run.PendingInterventions)-1].ConversationAnswer
		if receipt == nil || !reflect.DeepEqual(receipt.SelectedOptionIDs, []string{"o3", "o1"}) {
			t.Fatalf("receipt = %#v", receipt)
		}
		resume, _ := call.resume(t)
		if !resume.Resolved || call.entry.State != models.ToolEntryCompleted {
			t.Fatalf("resume = %#v", resume)
		}
		value := askUserResultValue(t, call.entry)
		if value["status"] != "answered" || value["answer"] != "Blog, Pricing" || value["messageId"] != answer.ID ||
			!reflect.DeepEqual(value["selectedOptions"], []interface{}{"Pricing", "Blog"}) {
			t.Fatalf("result = %#v", value)
		}
	})
}

func TestAskUserWithoutOptionsResultHasNoSelectedOptions(t *testing.T) {
	store := conversationtest.NewStore(t)
	f := claimedForegroundRun(t, store)
	parked, call := askUserParked(t, store, f.run, "foreground-worker", askUserTestQuestion)
	f.run = parked
	question := f.question(t)
	if question.Question != nil {
		t.Fatalf("free-text question carries options: %#v", question.Question)
	}
	answer := f.post(t, conversation.PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: models.MessageIntentAnswer,
		Content: "The pricing page.", ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
	if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID); err != nil {
		t.Fatal(err)
	}
	call.resume(t)
	if value := askUserResultValue(t, call.entry); value["answer"] != "The pricing page." || value["selectedOptions"] != nil {
		t.Fatalf("result = %#v", value)
	}
}

func TestPostChannelMessageRefusesInvalidSelectedOptions(t *testing.T) {
	store := conversationtest.NewStore(t)
	f := claimedForegroundRun(t, store)
	single := `{"question":"` + askUserTestQuestion + `","options":[{"label":"Pricing"},{"label":"Docs"}]}`
	call := newAskUserCallWithArguments(t, store, f.run, "foreground-worker", json.RawMessage(single))
	result := call.settle(t)
	f.run = call.park(t, result.Park)
	question := f.question(t)

	post := func(req conversation.PostChannelMessageRequest) error {
		current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		req.Scope, req.ConversationID, req.ExpectedRevision = current.Scope, current.ID, current.Revision
		req.Sender, req.Intent, req.Audience = f.trigger.Sender, models.MessageIntentAnswer, models.ConversationAudience{Kind: models.ConversationAudienceChannel}
		_, err = f.service.PostChannelMessage(t.Context(), req)
		return err
	}
	for name, req := range map[string]conversation.PostChannelMessageRequest{
		"unknown id":           {Content: "Blog", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o9"}},
		"two ids, one allowed": {Content: "Pricing, Docs", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o1", "o2"}},
		"duplicate ids":        {Content: "Pricing", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o1", "o1"}},
		"no question target":   {Content: "Pricing", SelectedOptionIDs: []string{"o1"}},
		"target offers none":   {Content: "Pricing", ReplyToMessageID: f.trigger.ID, SelectedOptionIDs: []string{"o1"}},
	} {
		req.IdempotencyKey = "bad-" + strings.ReplaceAll(name, " ", "-")
		if err := post(req); !errors.Is(err, models.ErrInvalidConversation) {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	// One option by reply id, and free text with nothing selected, are fine.
	if err := post(conversation.PostChannelMessageRequest{Content: "Docs", ReplyToMessageID: question.ID, SelectedOptionIDs: []string{"o2"}, IdempotencyKey: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := post(conversation.PostChannelMessageRequest{Content: "Something else", ResolvesMessageID: question.ID, IdempotencyKey: "free"}); err != nil {
		t.Fatal(err)
	}

	// Options belong only on a question.
	current, _ := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	_, err := f.service.PostChannelMessage(t.Context(), conversation.PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: f.trigger.Sender, Intent: models.MessageIntentAnswer, Content: "Pick one", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		Question: &models.ChannelQuestion{Options: []models.ChannelQuestionOption{{ID: "o1", Label: "A"}, {ID: "o2", Label: "B"}}}, IdempotencyKey: "not-a-question",
	})
	if !errors.Is(err, models.ErrInvalidConversation) {
		t.Fatalf("options on an answer: err = %v", err)
	}
}
