package runtime

import (
	"testing"
)

func TestProjectConversationRunbookStartAcknowledgesDurableChild(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "conversation-runbook-start"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Agent work", IdempotencyKey: "agent-work-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Run it now.", "run-now")
	source := &AgentRun{
		ID: "conversation-run", Kind: RunKindConversation, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: trigger.ID},
	}
	child := &AgentRun{ID: "operation-run", Kind: RunKindAgentWork, Scope: scope, Owner: owner, AssignedAgentID: owner.ID}
	proposal := &TurnRunbookProposal{Entrypoint: "engage-now", Summary: "Run the on-demand engagement operation"}
	if err := projectConversationRunbookStart(ctx, store, source, proposal, child); err != nil {
		t.Fatal(err)
	}
	if err := projectConversationRunbookStart(ctx, store, source, proposal, child); err != nil {
		t.Fatal(err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Intent != MessageIntentUpdate || messages[1].ReplyToMessageID != trigger.ID ||
		messages[1].Content != "Started: Run the on-demand engagement operation" || len(messages[1].References) != 1 ||
		messages[1].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: child.ID}) {
		t.Fatalf("conversation Runbook start projection = %#v", messages)
	}
}
