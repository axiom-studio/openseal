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
	"github.com/axiom-studio/openseal/pkg/runtime/control/controltest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

func TestAskUserPostsQuestionAndParks(t *testing.T) {
	controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
		f := controltest.ClaimedForegroundRun(t, store)
		call := controltest.NewAskUserCall(t, store, f.Run, "foreground-worker", "  "+controltest.AskUserQuestion+"  ")
		result := call.Settle(t)
		if result.Kind != control.Parked || result.Park.Status != models.AgentRunStatusWaitingForEvent ||
			result.Park.Wake.Type != "user_message" || result.Park.Wake.Reference != call.Entry.ID {
			t.Fatalf("park = %#v", result)
		}
		if call.Entry.State != models.ToolEntryAwaitingEvent || call.Entry.Tool.Wait == nil || call.Entry.Tool.Wait.EventWaitID != call.Entry.ID {
			t.Fatalf("entry = %#v", call.Entry)
		}
		key := "conversation-clarification:" + f.Run.ID + ":" + call.Entry.ID
		meta, err := control.DecodeControlEntryMeta(call.Entry)
		if err != nil || meta == nil || meta.Kind != models.ControlToolAskUser || meta.QuestionMessageID != models.StableConversationID(f.Run.Scope, key, "message") {
			t.Fatalf("meta = %#v %v", meta, err)
		}
		question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.Run.Scope, f.Conversation.ID, key)
		if err != nil || question == nil || question.ID != meta.QuestionMessageID || question.Content != controltest.AskUserQuestion ||
			question.Intent != models.MessageIntentQuestion || !question.RequiresResponse || question.ReplyToMessageID != f.Trigger.ID ||
			question.Sender != (models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: f.Run.AssignedAgentID}) ||
			len(question.References) != 1 || question.References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: f.Run.ID}) {
			t.Fatalf("question = %#v %v", question, err)
		}
		parked := call.Park(t, result.Park)
		if parked.Status != models.AgentRunStatusWaitingForEvent || parked.WakeCondition == nil || parked.WakeCondition.Type != "user_message" ||
			parked.WakeCondition.Reference != call.Entry.ID || controltest.ParkedQuestionKey(parked) != key {
			t.Fatalf("parked run is not a question wait: %#v", parked)
		}
		// The reconciler replays the same message, never a second one.
		if _, err := f.Scheduler.ReconcileScope(t.Context(), f.Run.Scope); err != nil {
			t.Fatal(err)
		}
		messages, _ := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.Run.Scope, ConversationID: f.Conversation.ID, Limit: 100})
		if len(messages) != 2 {
			t.Fatalf("messages = %d, want trigger + question", len(messages))
		}
	})
}

func TestAskUserResumeWithoutAnswerRepostsAndReparks(t *testing.T) {
	store := kerneltest.NewStore(t)
	f := controltest.ClaimedForegroundRun(t, store)
	call := controltest.NewAskUserCall(t, store, f.Run, "foreground-worker", controltest.AskUserQuestion)
	// Crash after the entry transition, before the post and the park: the
	// next claim resumes the awaiting entry.
	cc := call.Context()
	cc.Arguments = &models.AskUserArgs{Question: controltest.AskUserQuestion}
	entry, err := call.Exec.TransitionControlEntry(t.Context(), cc, models.ToolEntryAwaitingEvent, &models.ToolEntryWait{EventWaitID: call.Entry.ID, Reason: control.AskUserWaitReason}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call.Entry = entry
	resumed, err := call.Exec.Resume(t.Context(), call.Context())
	if err != nil || resumed.Resolved || resumed.Park == nil || resumed.Park.Wake.Reference != entry.ID {
		t.Fatalf("resume = %#v %v", resumed, err)
	}
	key := conversation.ClarificationKey(f.Run.ID, entry.ID)
	question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.Run.Scope, f.Conversation.ID, key)
	if err != nil || question == nil || question.Content != controltest.AskUserQuestion {
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
	controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
		f := controltest.ClaimedForegroundRun(t, store)
		call := controltest.NewAskUserCall(t, store, f.Run, "foreground-worker", controltest.AskUserQuestion)
		call.Exec.Conversations = conversation.NewService(conflictingQuestionStore{store})
		result := call.Settle(t)
		if result.Kind != control.Parked {
			t.Fatalf("settle = %#v", result)
		}
		key := conversation.ClarificationKey(f.Run.ID, call.Entry.ID)
		if q, _ := store.FindChannelMessageByIdempotencyKey(t.Context(), f.Run.Scope, f.Conversation.ID, key); q != nil {
			t.Fatal("question posted despite conflicts")
		}
		f.Run = call.Park(t, result.Park)
		if f.Run.Output["summary"] != nil {
			t.Fatal("question text leaked into Run output")
		}
		question := f.Question(t)
		if question.Content != controltest.AskUserQuestion || question.ID != models.StableConversationID(f.Run.Scope, key, "message") {
			t.Fatalf("reconciled question = %#v", question)
		}
	})
}

func TestAskUserAnswerFromOtherActorIgnored(t *testing.T) {
	store := kerneltest.NewStore(t)
	f := controltest.ClaimedForegroundRun(t, store)
	parked, call := controltest.AskUserParked(t, store, f.Run, "foreground-worker", controltest.AskUserQuestion)
	f.Run = parked
	question := f.Question(t)
	foreign := f.Post(t, conversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "someone-else"},
		Intent: models.MessageIntentAnswer, Content: "Use the admin page.", ResolvesMessageID: question.ID, IdempotencyKey: "foreign-answer"})
	// An explicit reply to the question is a clarification attempt, but a
	// foreign actor's answer wakes nothing and starts nothing.
	if result, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, foreign.ID); err != nil || result != nil {
		t.Fatalf("foreign answer accepted: %#v %v", result, err)
	}
	current := f.Current(t)
	if current.Status != models.AgentRunStatusWaitingForEvent || len(current.PendingInterventions) != 0 {
		t.Fatalf("foreign answer woke the Run: %#v", current)
	}
	if answer := control.AskUserAnswer(current, call.Entry, question.ID); answer != nil {
		t.Fatalf("foreign answer bound to the entry: %#v", answer)
	}
}

const askUserOptionsArguments = `{"question":"` + controltest.AskUserQuestion + `","allowMultiple":true,` +
	`"options":[{"label":"Pricing"},{"label":"Docs","description":"The developer docs"},{"label":"Blog"}]}`

func TestAskUserOptionsPostedAndAnsweredWithSelectedOptions(t *testing.T) {
	controltest.ForEachStore(t, func(t *testing.T, store controltest.Store) {
		f := controltest.ClaimedForegroundRun(t, store)
		call := controltest.NewAskUserCallWithArguments(t, store, f.Run, "foreground-worker", json.RawMessage(askUserOptionsArguments))
		result := call.Settle(t)
		if result.Kind != control.Parked {
			t.Fatalf("settle = %#v", result)
		}
		f.Run = call.Park(t, result.Park)
		// The reconciler replays the same request, options included.
		question := f.Question(t)
		want := &models.ChannelQuestion{AllowMultiple: true, Options: []models.ChannelQuestionOption{
			{ID: "o1", Label: "Pricing"}, {ID: "o2", Label: "Docs", Description: "The developer docs"}, {ID: "o3", Label: "Blog"},
		}}
		if !reflect.DeepEqual(question.Question, want) || question.Content != controltest.AskUserQuestion {
			t.Fatalf("question = %#v", question.Question)
		}

		answer := f.Post(t, conversation.PostChannelMessageRequest{Sender: f.Trigger.Sender, Intent: models.MessageIntentAnswer,
			Content: "Blog, Pricing", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o3", "o1"}, IdempotencyKey: "answer"})
		if !reflect.DeepEqual(answer.SelectedOptionIDs, []string{"o3", "o1"}) {
			t.Fatalf("answer = %#v", answer)
		}
		resumed, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, answer.ID)
		if err != nil || resumed == nil || resumed.Run.ID != f.Run.ID {
			t.Fatalf("answer did not wake the Run: %#v %v", resumed, err)
		}
		receipt := resumed.Run.PendingInterventions[len(resumed.Run.PendingInterventions)-1].ConversationAnswer
		if receipt == nil || !reflect.DeepEqual(receipt.SelectedOptionIDs, []string{"o3", "o1"}) {
			t.Fatalf("receipt = %#v", receipt)
		}
		resume, _ := call.Resume(t)
		if !resume.Resolved || call.Entry.State != models.ToolEntryCompleted {
			t.Fatalf("resume = %#v", resume)
		}
		value := controltest.AskUserResultValue(t, call.Entry)
		if value["status"] != "answered" || value["answer"] != "Blog, Pricing" || value["messageId"] != answer.ID ||
			!reflect.DeepEqual(value["selectedOptions"], []interface{}{"Pricing", "Blog"}) {
			t.Fatalf("result = %#v", value)
		}
	})
}

func TestAskUserWithoutOptionsResultHasNoSelectedOptions(t *testing.T) {
	store := kerneltest.NewStore(t)
	f := controltest.ClaimedForegroundRun(t, store)
	parked, call := controltest.AskUserParked(t, store, f.Run, "foreground-worker", controltest.AskUserQuestion)
	f.Run = parked
	question := f.Question(t)
	if question.Question != nil {
		t.Fatalf("free-text question carries options: %#v", question.Question)
	}
	answer := f.Post(t, conversation.PostChannelMessageRequest{Sender: f.Trigger.Sender, Intent: models.MessageIntentAnswer,
		Content: "The pricing page.", ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
	if _, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Run.Scope, f.Conversation.ID, answer.ID); err != nil {
		t.Fatal(err)
	}
	call.Resume(t)
	if value := controltest.AskUserResultValue(t, call.Entry); value["answer"] != "The pricing page." || value["selectedOptions"] != nil {
		t.Fatalf("result = %#v", value)
	}
}

func TestPostChannelMessageRefusesInvalidSelectedOptions(t *testing.T) {
	store := kerneltest.NewStore(t)
	f := controltest.ClaimedForegroundRun(t, store)
	single := `{"question":"` + controltest.AskUserQuestion + `","options":[{"label":"Pricing"},{"label":"Docs"}]}`
	call := controltest.NewAskUserCallWithArguments(t, store, f.Run, "foreground-worker", json.RawMessage(single))
	result := call.Settle(t)
	f.Run = call.Park(t, result.Park)
	question := f.Question(t)

	post := func(req conversation.PostChannelMessageRequest) error {
		current, err := f.Service.GetConversation(t.Context(), f.Conversation.Scope, f.Conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		req.Scope, req.ConversationID, req.ExpectedRevision = current.Scope, current.ID, current.Revision
		req.Sender, req.Intent, req.Audience = f.Trigger.Sender, models.MessageIntentAnswer, models.ConversationAudience{Kind: models.ConversationAudienceChannel}
		_, err = f.Service.PostChannelMessage(t.Context(), req)
		return err
	}
	for name, req := range map[string]conversation.PostChannelMessageRequest{
		"unknown id":           {Content: "Blog", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o9"}},
		"two ids, one allowed": {Content: "Pricing, Docs", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o1", "o2"}},
		"duplicate ids":        {Content: "Pricing", ResolvesMessageID: question.ID, SelectedOptionIDs: []string{"o1", "o1"}},
		"no question target":   {Content: "Pricing", SelectedOptionIDs: []string{"o1"}},
		"target offers none":   {Content: "Pricing", ReplyToMessageID: f.Trigger.ID, SelectedOptionIDs: []string{"o1"}},
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
	current, _ := f.Service.GetConversation(t.Context(), f.Conversation.Scope, f.Conversation.ID)
	_, err := f.Service.PostChannelMessage(t.Context(), conversation.PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: f.Trigger.Sender, Intent: models.MessageIntentAnswer, Content: "Pick one", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		Question: &models.ChannelQuestion{Options: []models.ChannelQuestionOption{{ID: "o1", Label: "A"}, {ID: "o2", Label: "B"}}}, IdempotencyKey: "not-a-question",
	})
	if !errors.Is(err, models.ErrInvalidConversation) {
		t.Fatalf("options on an answer: err = %v", err)
	}
}
