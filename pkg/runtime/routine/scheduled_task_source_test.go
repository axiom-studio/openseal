package routine_test

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
)

// A Run woken by a host notice (an account connected from a setup card) makes
// its routine report to the person's request the notice answers, not to the
// notice.
func TestScheduledTaskFromSetupNoticeReportsToPersonsRequest(t *testing.T) {
	store := routinetest.NewFileStore(t, "sqlite")
	run, _, _, args := routinetest.ScheduledTask(t, store)
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	requestID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversations := store.(models.ConversationStore)
	service := conversation.NewService(conversations)
	person := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user"}
	setup := models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "skill-setup-coordinator"}
	post := func(key, replyTo string) string {
		t.Helper()
		c, err := conversations.GetConversation(t.Context(), run.Scope, conversationID)
		if err != nil {
			t.Fatal(err)
		}
		posted, err := service.PostChannelMessage(t.Context(), conversation.PostChannelMessageRequest{
			Scope: run.Scope, ConversationID: conversationID, ExpectedRevision: c.Revision, Sender: setup, Initiator: &person,
			Intent: models.MessageIntentUpdate, Content: "Google Workspace connected", ReplyToMessageID: replyTo,
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return posted.Message.ID
	}
	card := post("setup-card", requestID)
	notice := post("setup-connected", card)

	woken := *run
	woken.Context = map[string]interface{}{}
	for key, value := range run.Context {
		woken.Context[key] = value
	}
	woken.Context[models.ConversationRunTriggerIDContextKey] = notice
	_, task, _, err := routine.ResolveScheduledTaskCreate(t.Context(), store.(routine.RunbookActionStore), nil, routine.RoutineEventActions{}, &woken, "", args)
	if err != nil {
		t.Fatal(err)
	}
	if task.SourceMessageID != requestID {
		t.Fatalf("routine source = %q, want the person's request %q", task.SourceMessageID, requestID)
	}
}
