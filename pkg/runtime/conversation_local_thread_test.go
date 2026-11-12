package runtime

import (
	"testing"

	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func TestLocalConversationThreadScopesFollowUpsAndPreservesIndependentWork(t *testing.T) {
	store := newTestKernelStore(t)
	service := runtimeconversation.NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "local-threads"}
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "local-threads",
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(key, root string, sender ConversationParticipantType) *ChannelMessage {
		t.Helper()
		latest, err := service.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		intent := MessageIntentQuestion
		if sender == ConversationParticipantService {
			intent = MessageIntentSystem
		}
		result, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: latest.Revision,
			Sender: ConversationParticipant{Type: sender, ID: "user"}, Intent: intent, Content: key,
			RequiresResponse: sender == ConversationParticipantUser,
			Audience:         ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: root, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Message
	}
	rootOne := post("call-one", "", ConversationParticipantService)
	rootTwo := post("call-two", "", ConversationParticipantService)
	scheduler := mustConversationRunScheduler(t, store)
	schedule := func(message *ChannelMessage) *AgentRun {
		t.Helper()
		result, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
		if err != nil || result == nil || result.Run == nil {
			t.Fatalf("schedule message: %#v %v", result, err)
		}
		return result.Run
	}
	ordinary := schedule(post("ordinary request", "", ConversationParticipantUser))
	one := schedule(post("first call request", rootOne.ID, ConversationParticipantUser))
	two := schedule(post("other call request", rootTwo.ID, ConversationParticipantUser))
	independent, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Kind: RunKindAgentWork, Owner: conversation.Owner, AssignedAgentID: "agent", Goal: "Continue independent work",
		Source: RunSourceChat, ConcurrencyKey: "task:independent", Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, "threadRootMessageId": rootOne.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	followUp := post("follow up on first call reply", rootOne.ID, ConversationParticipantUser)
	if delivered := schedule(followUp); delivered.ID != one.ID {
		t.Fatalf("thread follow-up started another Run: %#v", delivered)
	}
	for _, expected := range []struct {
		run      *AgentRun
		followUp string
	}{
		{ordinary, ""}, {one, followUp.ID}, {two, ""}, {independent, ""},
	} {
		want := 0
		if expected.followUp != "" {
			want = 1
		}
		current, err := store.GetAgentRun(t.Context(), scope, expected.run.ID)
		if err != nil || current.Status != AgentRunStatusQueued || len(conversationRunFollowUps(current)) != want ||
			want == 1 && !conversationRunHasFollowUp(current, expected.followUp) {
			t.Fatalf("follow-up reached the wrong thread: %#v expected=%q err=%v", current, expected.followUp, err)
		}
	}
	if one.Context["threadRootMessageId"] != rootOne.ID || one.ConcurrencyKey != ConversationRunLane(conversation.ID, rootOne.ID, one.AssignedAgentID) || ordinary.ConcurrencyKey != ConversationRunLane(conversation.ID, "", ordinary.AssignedAgentID) {
		t.Fatal("local roots did not become canonical foreground execution context")
	}
	unthreaded := post("refine ordinary request", "", ConversationParticipantUser)
	if delivered := schedule(unthreaded); delivered.ID != ordinary.ID || !conversationRunHasFollowUp(delivered, unthreaded.ID) {
		t.Fatalf("unthreaded follow-up was not delivered to the conversation lane Run: %#v", delivered)
	}
	current, err := store.GetAgentRun(t.Context(), scope, one.ID)
	if err != nil || conversationRunHasFollowUp(current, unthreaded.ID) {
		t.Fatal("unthreaded prompt reached another call's Run")
	}
}
