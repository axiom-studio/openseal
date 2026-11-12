package runtime

import (
	"context"
	"testing"
	"time"
)

func TestActiveConversationRunsIncludeNonterminalDescendantAfterCoordinationRootEnds(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "1"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	root := &AgentRun{
		ID: "old-conversation", Kind: RunKindConversation, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Respond to a channel request", Source: RunSourceChat, Status: AgentRunStatusCompleted,
		RootRunID: "old-conversation", Revision: 3, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
		Context: map[string]interface{}{conversationRunContextConversationID: "channel-1"},
	}
	child := &AgentRun{
		ID: "still-working", Kind: RunKindAgentWork, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		ParentRunID: root.ID, RootRunID: root.ID, Goal: "Await review", Source: RunSourceFork,
		Status: AgentRunStatusWaitingForApproval, Revision: 2, CreatedAt: now, UpdatedAt: now,
		WakeCondition: &WakeCondition{Type: "approval", Reference: "approval-1"},
	}
	if err := store.CreateAgentRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAgentRun(ctx, child); err != nil {
		t.Fatal(err)
	}
	runner := &ConversationRunTurnRunner{portfolio: store}
	active, err := runner.activeConversationRuns(ctx, &Conversation{ID: "channel-1", Scope: scope, Owner: owner})
	if err != nil || len(active) != 1 || active[0].ID != child.ID {
		t.Fatalf("active Runs = %#v, %v", active, err)
	}
}

func TestActiveConversationRunsIncludeOnlyExactIndependentTaskThreadAndActor(t *testing.T) {
	store := newTestKernelStore(t)
	source, turn, conversation, trigger := settledTaskSourceFixture(t, store)
	if _, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(t, store, source, turn)); err != nil {
		t.Fatal(err)
	}
	runner := &ConversationRunTurnRunner{portfolio: store}
	active, err := runner.activeConversationRuns(t.Context(), conversation, trigger)
	if err != nil || len(active) != 1 || active[0].Revision < 1 || !slicesContainsRunCommand(active[0].AvailableControls, RunActionCancel) {
		t.Fatalf("task controls absent from authoritative thread snapshot: %#v %v", active, err)
	}
	// Provider channels scope controls to their exact thread; ordinary app
	// conversations retain task visibility for later messages in the same chat.
	conversation.Origin = &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "external-channel"}
	for _, change := range []func(*ChannelMessage){
		func(message *ChannelMessage) { message.Sender.ID = "other-user" },
		func(message *ChannelMessage) { message.ThreadRootID = "other-thread" },
	} {
		other := cloneChannelMessage(trigger)
		change(other)
		active, err := runner.activeConversationRuns(t.Context(), conversation, other)
		if err != nil || len(active) != 0 {
			t.Fatalf("task leaked across actor or thread: %#v %v", active, err)
		}
	}
}
