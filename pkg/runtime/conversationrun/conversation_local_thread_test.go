package conversationrun

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestLocalConversationThreadScopesFollowUpsAndPreservesIndependentWork(t *testing.T) {
	store := kerneltest.NewStore(t)
	service := runtimeconversation.NewService(store)
	scope := models.Scope{Kind: "tenant", ID: "local-threads"}
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "local-threads",
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(key, root string, sender models.ConversationParticipantType) *models.ChannelMessage {
		t.Helper()
		latest, err := service.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		intent := models.MessageIntentQuestion
		if sender == models.ConversationParticipantService {
			intent = models.MessageIntentSystem
		}
		result, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: latest.Revision,
			Sender: models.ConversationParticipant{Type: sender, ID: "user"}, Intent: intent, Content: key,
			RequiresResponse: sender == models.ConversationParticipantUser,
			Audience:         models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: root, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Message
	}
	rootOne := post("call-one", "", models.ConversationParticipantService)
	rootTwo := post("call-two", "", models.ConversationParticipantService)
	scheduler := newTestScheduler(t, store)
	schedule := func(message *models.ChannelMessage) *models.AgentRun {
		t.Helper()
		result, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
		if err != nil || result == nil || result.Run == nil {
			t.Fatalf("schedule message: %#v %v", result, err)
		}
		return result.Run
	}
	ordinary := schedule(post("ordinary request", "", models.ConversationParticipantUser))
	one := schedule(post("first call request", rootOne.ID, models.ConversationParticipantUser))
	two := schedule(post("other call request", rootTwo.ID, models.ConversationParticipantUser))
	independent, err := portfolio.NewService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindAgentWork, Owner: conversation.Owner, AssignedAgentID: "agent", Goal: "Continue independent work",
		Source: models.RunSourceChat, ConcurrencyKey: "task:independent", Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID, "threadRootMessageId": rootOne.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	followUp := post("follow up on first call reply", rootOne.ID, models.ConversationParticipantUser)
	if delivered := schedule(followUp); delivered.ID != one.ID {
		t.Fatalf("thread follow-up started another Run: %#v", delivered)
	}
	for _, expected := range []struct {
		run      *models.AgentRun
		followUp string
	}{
		{ordinary, ""}, {one, followUp.ID}, {two, ""}, {independent, ""},
	} {
		want := 0
		if expected.followUp != "" {
			want = 1
		}
		current, err := store.GetAgentRun(t.Context(), scope, expected.run.ID)
		if err != nil || current.Status != models.AgentRunStatusQueued || len(models.ConversationRunFollowUps(current)) != want ||
			want == 1 && !conversationRunHasFollowUp(current, expected.followUp) {
			t.Fatalf("follow-up reached the wrong thread: %#v expected=%q err=%v", current, expected.followUp, err)
		}
	}
	if one.Context["threadRootMessageId"] != rootOne.ID || one.ConcurrencyKey != models.ConversationRunLane(conversation.ID, rootOne.ID, one.AssignedAgentID) || ordinary.ConcurrencyKey != models.ConversationRunLane(conversation.ID, "", ordinary.AssignedAgentID) {
		t.Fatal("local roots did not become canonical foreground execution context")
	}
	unthreaded := post("refine ordinary request", "", models.ConversationParticipantUser)
	if delivered := schedule(unthreaded); delivered.ID != ordinary.ID || !conversationRunHasFollowUp(delivered, unthreaded.ID) {
		t.Fatalf("unthreaded follow-up was not delivered to the conversation lane Run: %#v", delivered)
	}
	current, err := store.GetAgentRun(t.Context(), scope, one.ID)
	if err != nil || conversationRunHasFollowUp(current, unthreaded.ID) {
		t.Fatal("unthreaded prompt reached another call's Run")
	}
}
