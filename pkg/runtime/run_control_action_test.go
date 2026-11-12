package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestRunActionValidatorResolvesRevisionAndEnforcesConversationOwner(t *testing.T) {
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "1"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "engage", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := agentrun.RunManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: definition.Actions[models.RunActionCancel]}
	validator, err := agentrun.NewRunActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	conversationRun := &models.AgentRun{ID: "conversation", Scope: scope, Owner: owner, Kind: models.RunKindConversation}
	arguments, handled, err := validator.ResolveActionProposalArguments(ctx, agentrun.ActionProposalValidationInput{
		Run: conversationRun, Bound: bound, Arguments: map[string]interface{}{"runId": created.Run.ID, "reason": "stop duplicate work"},
	})
	if err != nil || !handled || arguments["expectedRevision"] != created.Run.Revision {
		t.Fatalf("resolved = %#v, %v, %v", arguments, handled, err)
	}
	if _, err = validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: conversationRun, Bound: bound, Arguments: arguments}); err != nil {
		t.Fatal(err)
	}
	foreign := *conversationRun
	foreign.Owner = models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-2"}
	if _, err = validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: &foreign, Bound: bound, Arguments: arguments}); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("foreign owner error = %v", err)
	}
}

func TestRunActionDispatcherCancelsWaitingApprovalRun(t *testing.T) {
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "1"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "engage", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := agentrun.RunManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: definition.Actions[models.RunActionCancel]}
	dispatcher, err := agentrun.NewRunActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{
		Run: &models.AgentRun{Scope: scope, Owner: owner}, Bound: bound,
		Call: &models.ActionCall{Scope: scope, Arguments: map[string]interface{}{
			"runId": created.Run.ID, "expectedRevision": created.Run.Revision, "reason": "user canceled it",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := conversationResultMap(output["run"])
	if result["status"] != string(models.AgentRunStatusCanceled) && result["status"] != models.AgentRunStatusCanceled {
		t.Fatalf("output = %#v", output)
	}
}

func TestApplicableAgentRunCommandsAreTruthful(t *testing.T) {
	if got := agentrun.ApplicableAgentRunCommands(&models.AgentRun{Kind: models.RunKindAgentWork, Status: models.AgentRunStatusWaitingForApproval}); len(got) != 2 || got[0] != models.RunActionPause || got[1] != models.RunActionCancel {
		t.Fatalf("waiting controls = %#v", got)
	}
	if got := agentrun.ApplicableAgentRunCommands(&models.AgentRun{Kind: models.RunKindAgentWork, Status: models.AgentRunStatusPaused}); len(got) != 2 || got[0] != models.RunActionResume || got[1] != models.RunActionCancel {
		t.Fatalf("paused controls = %#v", got)
	}
	if got := agentrun.ApplicableAgentRunCommands(&models.AgentRun{Kind: models.RunKindAgentWork, Status: models.AgentRunStatusCompleted}); len(got) != 0 {
		t.Fatalf("terminal controls = %#v", got)
	}
}
