package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestConversationRunControlsExposeOnlyCurrentExactRunIDs(t *testing.T) {
	definition := RunManagementSkill()
	action := capability.ModelAction{
		Name: "openseal.runs.cancel", SkillID: RunManagementSkillID, Version: RunManagementSkillVersion,
		Action: RunActionCancel, InputSchema: definition.Actions[RunActionCancel].InputSchema,
	}
	projected := constrainConversationRunActions([]capability.ModelAction{action}, []agentConversationActiveRun{
		{ID: "run-current", AvailableControls: []string{RunActionPause, RunActionCancel}},
		{ID: "run-paused", AvailableControls: []string{RunActionResume, RunActionCancel}},
	})
	if len(projected) != 1 {
		t.Fatalf("projected actions = %#v", projected)
	}
	properties := projected[0].InputSchema["properties"].(map[string]interface{})
	values := properties["runId"].(map[string]interface{})["enum"].([]interface{})
	if len(values) != 2 || values[0] != "run-current" || values[1] != "run-paused" {
		t.Fatalf("Run id choices = %#v", values)
	}
	if _, leaked := definition.Actions[RunActionCancel].InputSchema["properties"].(map[string]interface{})["runId"].(map[string]interface{})["enum"]; leaked {
		t.Fatal("conversation projection mutated the canonical Skill schema")
	}
}

func TestConversationOperationOutcomeUsesDurableChildInsteadOfSecondModelNarration(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "1"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	root := &AgentRun{
		ID: "conversation", Kind: RunKindConversation, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Respond to a channel request", Source: RunSourceChat, Status: AgentRunStatusRunning,
		RootRunID: "conversation", LastAppliedTurn: 1, Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	child := &AgentRun{
		ID: "operation", Kind: RunKindAgentWork, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		ParentRunID: root.ID, RootRunID: root.ID, Entrypoint: "ondemand-engage",
		Goal: "Run on-demand engagement", Source: RunSourceFork, Status: AgentRunStatusCompleted,
		Revision: 3, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	if err := store.CreateAgentRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAgentRun(ctx, child); err != nil {
		t.Fatal(err)
	}
	runner := &ConversationRunTurnRunner{portfolio: store}
	outcome, ok, err := runner.governedConversationOperationOutcome(ctx, root)
	if err != nil || !ok {
		t.Fatalf("operation outcome = %#v, %v, %v", outcome, ok, err)
	}
	if outcome.ResourceID != child.ID || outcome.Content != "Operation “ondemand-engage” completed successfully." ||
		len(outcome.References) != 1 || outcome.References[0].ID != child.ID {
		t.Fatalf("operation outcome = %#v", outcome)
	}
}

func TestActiveConversationRunsIncludeNonterminalDescendantAfterCoordinationRootEnds(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
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
