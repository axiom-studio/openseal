package runtime

import (
	"context"
	"reflect"
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

func TestObjectiveConversationReplacesRetiredRunbookScheduleWithFreshActivation(t *testing.T) {
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
	source, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "exhausted-hourly-community-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "2.0.19", TriggerID: "hourly", Status: RunbookActivationRetired,
		Trigger: runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "review",
			Schedule:  &runbook.Schedule{Cron: "0 43 * * * *", Timezone: "UTC", JitterSeconds: 17, MaximumOccurrences: 7},
			Reporting: &runbook.ReportingPolicy{Channel: "hourly-work", Title: "Hourly work", Milestones: []runbook.ReportingMilestone{runbook.ReportingStarted, runbook.ReportingApprovalRequired, runbook.ReportingCompleted, runbook.ReportingFailed}},
		},
		Input: map[string]interface{}{"communities": []interface{}{"r/saas"}}, Policy: map[string]interface{}{"approvalTimeoutSeconds": float64(900)},
		Budget: &BudgetPolicy{MaxTurns: 100, MaxActions: 100}, MaximumConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: objective.Title,
		Origin: &ConversationReference{Kind: ConversationReferenceObjective, ID: objective.ID, Version: objective.Revision}, IdempotencyKey: "objective-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationRun, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Run hourly for five hours", Source: RunSourceChat,
		Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	definition := RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: definition.Actions[RunbookActionReplaceSchedule]}
	validator, _ := NewRunbookActionValidator(store)
	resolved, handled, err := validator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{
		Run: conversationRun, Bound: bound, Arguments: map[string]interface{}{"maximumOccurrences": float64(5), "reason": "Run five hourly acceptance cycles"},
	})
	if err != nil || !handled || resolved["activationId"] != source.ID {
		t.Fatalf("resolve replacement: resolved=%#v handled=%v err=%v", resolved, handled, err)
	}
	preview, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: conversationRun, Bound: bound, Arguments: resolved})
	if err != nil {
		t.Fatal(err)
	}
	replacementSchedule, _ := preview["replacementSchedule"].(*runbook.Schedule)
	if replacementSchedule == nil || replacementSchedule.MaximumOccurrences != 5 || replacementSchedule.Cron != source.Trigger.Schedule.Cron || preview["sourceActivationId"] != source.ID {
		t.Fatalf("replacement preview = %#v", preview)
	}
	dispatcher, _ := NewRunbookActionDispatcher(store, nil)
	call := &ActionCall{ID: "replace-five-hour-schedule", Scope: scope, RunID: conversationRun.ID, DeploymentID: owner.ID}
	result, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: call, Run: conversationRun, Bound: bound, Arguments: resolved})
	if err != nil {
		t.Fatal(err)
	}
	replacement, _ := result["activation"].(*RunbookActivation)
	if replacement == nil || replacement.ID == source.ID || replacement.Status != RunbookActivationActive || replacement.OccurrencesProcessed != 0 ||
		replacement.NextRunAt != nil || replacement.NextOccurrenceBase != nil || replacement.Trigger.Schedule.MaximumOccurrences != 5 || result["replayed"] != false {
		t.Fatalf("replacement result = %#v", result)
	}
	if replacement.DefinitionID != source.DefinitionID || replacement.DefinitionVersion != source.DefinitionVersion || replacement.TriggerID != source.TriggerID ||
		replacement.Trigger.Entrypoint != source.Trigger.Entrypoint || replacement.MaximumConcurrent != source.MaximumConcurrent ||
		!reflect.DeepEqual(replacement.Input, source.Input) || !reflect.DeepEqual(replacement.Policy, source.Policy) ||
		!reflect.DeepEqual(replacement.Budget, source.Budget) || !reflect.DeepEqual(replacement.Trigger.Reporting, source.Trigger.Reporting) {
		t.Fatalf("replacement changed reviewed execution contract: source=%#v replacement=%#v", source, replacement)
	}
	unchanged, _ := store.GetRunbookActivation(ctx, scope, source.ID)
	if unchanged == nil || unchanged.Status != RunbookActivationRetired || unchanged.Revision != source.Revision || unchanged.Trigger.Schedule.MaximumOccurrences != 7 {
		t.Fatalf("retired source was mutated: %#v", unchanged)
	}
	replayed, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: call, Run: conversationRun, Bound: bound, Arguments: resolved})
	if err != nil || replayed["replayed"] != true || replayed["activation"].(*RunbookActivation).ID != replacement.ID {
		t.Fatalf("replacement replay = %#v err=%v", replayed, err)
	}
}

func TestRunbookScheduleReplacementRejectsActiveOrForeignActivation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	rowan := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	other := ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: other, Title: "Other work", Goal: "Do other work", Status: ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "other-retired-runbook", Scope: scope, Owner: other, ObjectiveID: objective.ID, AssignedAgentID: other.ID,
		DefinitionID: "other", DefinitionVersion: "1", TriggerID: "daily", Status: RunbookActivationRetired,
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC", MaximumOccurrences: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: rowan.ID}, Action: definition.Actions[RunbookActionReplaceSchedule]}
	validator, _ := NewRunbookActionValidator(store)
	_, err = validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
		Run: &AgentRun{Scope: scope, Owner: rowan, AssignedAgentID: rowan.ID}, Bound: bound,
		Arguments: map[string]interface{}{"activationId": foreign.ID, "maximumOccurrences": float64(5), "reason": "Try foreign work"},
	})
	if err == nil {
		t.Fatal("foreign Runbook schedule replacement was accepted")
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
