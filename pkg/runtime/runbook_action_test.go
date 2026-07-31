package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestObjectiveConversationResolvesObjectiveMutationAndRunbookStart(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Community participation", Goal: "Contribute useful advice", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "hourly-community-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "hourly",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: objective.Title,
		Origin:         &ConversationReference{Kind: ConversationReferenceObjective, ID: objective.ID, Version: objective.Revision},
		IdempotencyKey: "objective-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationRun, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Run this objective now", Source: RunSourceChat,
		Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	objectiveDefinition := ObjectiveManagementSkill()
	objectiveBound := &skill.BoundAction{Definition: objectiveDefinition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: objectiveDefinition.Actions[ObjectiveActionUpdate]}
	objectiveValidator, err := NewObjectiveActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	resolvedObjective, handled, err := objectiveValidator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{
		Run: conversationRun, Bound: objectiveBound, Arguments: map[string]interface{}{"goal": "Scan more communities"},
	})
	if err != nil || !handled {
		t.Fatalf("resolve Objective arguments: handled=%v err=%v", handled, err)
	}
	if resolvedObjective["objectiveId"] != objective.ID || resolvedObjective["expectedRevision"] != objective.Revision {
		t.Fatalf("resolved Objective arguments = %#v", resolvedObjective)
	}

	runbookDefinition := RunbookManagementSkill()
	runbookBound := &skill.BoundAction{Definition: runbookDefinition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: runbookDefinition.Actions[RunbookActionStart]}
	runbookValidator, err := NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRunbook, handled, err := runbookValidator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{
		Run: conversationRun, Bound: runbookBound, Arguments: map[string]interface{}{"reason": "User requested an off-cycle run"},
	})
	if err != nil || !handled {
		t.Fatalf("resolve Runbook arguments: handled=%v err=%v", handled, err)
	}
	if resolvedRunbook["activationId"] != activation.ID {
		t.Fatalf("resolved Runbook arguments = %#v", resolvedRunbook)
	}
	preview, err := runbookValidator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: conversationRun, Bound: runbookBound, Arguments: resolvedRunbook})
	if err != nil || preview["activationId"] != activation.ID || preview["objectiveId"] != objective.ID {
		t.Fatalf("Runbook preview=%#v err=%v", preview, err)
	}
	dispatcher, err := NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := &ActionCall{ID: "action-start-hourly", Scope: scope, RunID: conversationRun.ID, DeploymentID: owner.ID}
	result, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: call, Run: conversationRun, Bound: runbookBound, Arguments: resolvedRunbook})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := result["run"].(*AgentRun)
	if started == nil || started.ObjectiveID != objective.ID || started.Source != RunSourceManual || result["replayed"] != false {
		t.Fatalf("Runbook start result = %#v", result)
	}
	replayed, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: call, Run: conversationRun, Bound: runbookBound, Arguments: resolvedRunbook})
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("Runbook replay = %#v err=%v", replayed, err)
	}
}

func TestRunbookStartRejectsActivationOwnedByAnotherAgent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	rowan := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	other := ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: other, Title: "Other work", Goal: "Do other work", Status: ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "other-runbook", Scope: scope, Owner: other, ObjectiveID: objective.ID, AssignedAgentID: other.ID,
		DefinitionID: "other", DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{Scope: scope, Owner: rowan, AssignedAgentID: rowan.ID}
	definition := RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: rowan.ID}, Action: definition.Actions[RunbookActionStart]}
	validator, _ := NewRunbookActionValidator(store)
	_, err = validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
		Run: run, Bound: bound, Arguments: map[string]interface{}{"activationId": activation.ID, "reason": "Try foreign work"},
	})
	if err == nil {
		t.Fatal("foreign Runbook activation was accepted")
	}
}
