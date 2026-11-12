package conversationrun_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

func TestActiveConversationRunsIncludeNonterminalDescendantAfterCoordinationRootEnds(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	now := time.Now().UTC()
	scope := models.Scope{Kind: "tenant", ID: "1"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	root := &models.AgentRun{
		ID: "old-conversation", Kind: models.RunKindConversation, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Respond to a channel request", Source: models.RunSourceChat, Status: models.AgentRunStatusCompleted,
		RootRunID: "old-conversation", Revision: 3, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "channel-1"},
	}
	child := &models.AgentRun{
		ID: "still-working", Kind: models.RunKindAgentWork, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		ParentRunID: root.ID, RootRunID: root.ID, Goal: "Await review", Source: models.RunSourceFork,
		Status: models.AgentRunStatusWaitingForApproval, Revision: 2, CreatedAt: now, UpdatedAt: now,
		WakeCondition: &models.WakeCondition{Type: "approval", Reference: "approval-1"},
	}
	if err := store.CreateAgentRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAgentRun(ctx, child); err != nil {
		t.Fatal(err)
	}
	runner := conversationrun.NewPortfolioTurnRunner(store)
	active, err := runner.ActiveConversationRuns(ctx, &models.Conversation{ID: "channel-1", Scope: scope, Owner: owner})
	if err != nil || len(active) != 1 || active[0].ID != child.ID {
		t.Fatalf("active Runs = %#v, %v", active, err)
	}
}

func TestActiveConversationRunsIncludeOnlyExactIndependentTaskThreadAndActor(t *testing.T) {
	store := kerneltest.NewStore(t)
	source, turn, conversation, trigger := conversationruntest.SettledTaskSource(t, store)
	if _, err := conversationrun.NewConversationTaskService(store).Start(t.Context(), conversationruntest.TaskStartRequest(t, store, source, turn)); err != nil {
		t.Fatal(err)
	}
	runner := conversationrun.NewPortfolioTurnRunner(store)
	active, err := runner.ActiveConversationRuns(t.Context(), conversation, trigger)
	if err != nil || len(active) != 1 || active[0].Revision < 1 || !slices.Contains(active[0].AvailableControls, models.RunActionCancel) {
		t.Fatalf("task controls absent from authoritative thread snapshot: %#v %v", active, err)
	}
	// Provider channels scope controls to their exact thread; ordinary app
	// conversations retain task visibility for later messages in the same chat.
	conversation.Origin = &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: "external-channel"}
	for _, change := range []func(*models.ChannelMessage){
		func(message *models.ChannelMessage) { message.Sender.ID = "other-user" },
		func(message *models.ChannelMessage) { message.ThreadRootID = "other-thread" },
	} {
		other := models.CloneChannelMessage(trigger)
		change(other)
		active, err := runner.ActiveConversationRuns(t.Context(), conversation, other)
		if err != nil || len(active) != 0 {
			t.Fatalf("task leaked across actor or thread: %#v %v", active, err)
		}
	}
}
