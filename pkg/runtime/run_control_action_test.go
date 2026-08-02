package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestRunActionValidatorResolvesRevisionAndEnforcesConversationOwner(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "1"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	created, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "engage", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := RunManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: definition.Actions[RunActionCancel]}
	validator, err := NewRunActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	conversationRun := &AgentRun{ID: "conversation", Scope: scope, Owner: owner, Kind: RunKindConversation}
	arguments, handled, err := validator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{
		Run: conversationRun, Bound: bound, Arguments: map[string]interface{}{"runId": created.Run.ID, "reason": "stop duplicate work"},
	})
	if err != nil || !handled || arguments["expectedRevision"] != created.Run.Revision {
		t.Fatalf("resolved = %#v, %v, %v", arguments, handled, err)
	}
	if _, err = validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: conversationRun, Bound: bound, Arguments: arguments}); err != nil {
		t.Fatal(err)
	}
	foreign := *conversationRun
	foreign.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-2"}
	if _, err = validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: &foreign, Bound: bound, Arguments: arguments}); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("foreign owner error = %v", err)
	}
}

func TestRunActionDispatcherCancelsWaitingApprovalRun(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "1"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	created, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "engage", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := RunManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: definition.Actions[RunActionCancel]}
	dispatcher, err := NewRunActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{
		Run: &AgentRun{Scope: scope, Owner: owner}, Bound: bound,
		Call: &ActionCall{Scope: scope, Arguments: map[string]interface{}{
			"runId": created.Run.ID, "expectedRevision": created.Run.Revision, "reason": "user canceled it",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := conversationResultMap(output["run"])
	if result["status"] != string(AgentRunStatusCanceled) && result["status"] != AgentRunStatusCanceled {
		t.Fatalf("output = %#v", output)
	}
}

func TestApplicableAgentRunCommandsAreTruthful(t *testing.T) {
	if got := applicableAgentRunCommands(&AgentRun{Kind: RunKindAgentWork, Status: AgentRunStatusWaitingForApproval}); len(got) != 2 || got[0] != RunActionPause || got[1] != RunActionCancel {
		t.Fatalf("waiting controls = %#v", got)
	}
	if got := applicableAgentRunCommands(&AgentRun{Kind: RunKindAgentWork, Status: AgentRunStatusPaused}); len(got) != 2 || got[0] != RunActionResume || got[1] != RunActionCancel {
		t.Fatalf("paused controls = %#v", got)
	}
	if got := applicableAgentRunCommands(&AgentRun{Kind: RunKindAgentWork, Status: AgentRunStatusCompleted}); len(got) != 0 {
		t.Fatalf("terminal controls = %#v", got)
	}
}
