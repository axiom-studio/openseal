package runtime

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestRunbookScheduleReplacementContractRequiresTimingChange(t *testing.T) {
	ctx := context.Background()
	catalog := skill.NewCatalog()
	definition := routine.RunbookManagementSkill()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "runbooks", Revision: 1, Scope: skill.ScopeReference{Kind: "tenant", ID: "3"}, DeploymentID: "rowan",
		SkillID: definition.ID, SkillVersion: definition.Version,
		AllowedActions: []string{routine.RunbookActionReplaceSchedule}, MaximumRisk: skill.RiskLevelWrite,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, binding.Scope, binding.DeploymentID, definition.ID, definition.Version, routine.RunbookActionReplaceSchedule)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"reason": "resume hourly work"}); err == nil {
		t.Fatal("replacement without a timing change must fail schema validation")
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"reason": "hourly", "cron": "0 * * * *"}); err == nil {
		t.Fatal("replacement with a five-field cron must fail schema validation")
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"reason": "hourly", "cron": "0 0 * * * *"}); err != nil {
		t.Fatalf("replacement with a six-field cron: %v", err)
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"reason": "run five times", "maximumOccurrences": float64(5)}); err != nil {
		t.Fatalf("replacement with a timing change: %v", err)
	}
}

func TestObjectiveConversationResolvesObjectiveMutationAndRunbookStart(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := portfolio.NewPortfolioService(store).CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Community participation", Goal: "Contribute useful advice", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		ID: "hourly-community-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "hourly",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedTestSpace(t, store, scope)
	conversation, _, err := conversation.NewConversationService(store).CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: objective.Title,
		Origin:         &models.ConversationReference{Kind: models.ConversationReferenceObjective, ID: objective.ID, Version: objective.Revision},
		IdempotencyKey: "objective-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationRun, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Run this objective now", Source: models.RunSourceChat,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	objectiveDefinition := action.ObjectiveManagementSkill()
	objectiveBound := &skill.BoundAction{Definition: objectiveDefinition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: objectiveDefinition.Actions[action.ObjectiveActionUpdate]}
	objectiveValidator, err := action.NewObjectiveActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	resolvedObjective, handled, err := objectiveValidator.ResolveActionProposalArguments(ctx, agentrun.ActionProposalValidationInput{
		Run: conversationRun, Bound: objectiveBound, Arguments: map[string]interface{}{"goal": "Scan more communities"},
	})
	if err != nil || !handled {
		t.Fatalf("resolve Objective arguments: handled=%v err=%v", handled, err)
	}
	if resolvedObjective["objectiveId"] != objective.ID || resolvedObjective["expectedRevision"] != objective.Revision {
		t.Fatalf("resolved Objective arguments = %#v", resolvedObjective)
	}

	runbookDefinition := routine.RunbookManagementSkill()
	runbookBound := &skill.BoundAction{Definition: runbookDefinition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: runbookDefinition.Actions[routine.RunbookActionStart]}
	runbookValidator, err := routine.NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRunbook, handled, err := runbookValidator.ResolveActionProposalArguments(ctx, agentrun.ActionProposalValidationInput{
		Run: conversationRun, Bound: runbookBound, Arguments: map[string]interface{}{"reason": "User requested an off-cycle run"},
	})
	if err != nil || !handled {
		t.Fatalf("resolve Runbook arguments: handled=%v err=%v", handled, err)
	}
	if resolvedRunbook["activationId"] != activation.ID {
		t.Fatalf("resolved Runbook arguments = %#v", resolvedRunbook)
	}
	preview, err := runbookValidator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: conversationRun, Bound: runbookBound, Arguments: resolvedRunbook})
	if err != nil || preview["activationId"] != activation.ID || preview["objectiveId"] != objective.ID {
		t.Fatalf("Runbook preview=%#v err=%v", preview, err)
	}
	dispatcher, err := routine.NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := &models.ActionCall{ID: "action-start-hourly", Scope: scope, RunID: conversationRun.ID, DeploymentID: owner.ID}
	result, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: call, Run: conversationRun, Bound: runbookBound, Arguments: resolvedRunbook})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := result["run"].(*models.AgentRun)
	if started == nil || started.ObjectiveID != objective.ID || started.Source != models.RunSourceManual || result["replayed"] != false {
		t.Fatalf("Runbook start result = %#v", result)
	}
	replayed, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: call, Run: conversationRun, Bound: runbookBound, Arguments: resolvedRunbook})
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("Runbook replay = %#v err=%v", replayed, err)
	}
}

func TestObjectiveConversationResolvesAutomaticallyExhaustedRunbookStart(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := portfolio.NewPortfolioService(store).CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Community participation", Goal: "Contribute useful advice", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		ID: "exhausted-community-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "hourly",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC", MaximumOccurrences: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedRevision := activation.Revision
	activation.Status, activation.OccurrencesProcessed, activation.Revision = models.RunbookActivationRetired, 2, activation.Revision+1
	if err := store.UpdateRunbookActivation(ctx, activation, expectedRevision); err != nil {
		t.Fatal(err)
	}
	seedTestSpace(t, store, scope)
	conversation, _, err := conversation.NewConversationService(store).CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: objective.Title,
		Origin: &models.ConversationReference{Kind: models.ConversationReferenceObjective, ID: objective.ID, Version: objective.Revision}, IdempotencyKey: "objective-channel-exhausted",
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationRun, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Run this objective now", Source: models.RunSourceChat,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := routine.RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: definition.Actions[routine.RunbookActionStart]}
	validator, err := routine.NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	resolved, handled, err := validator.ResolveActionProposalArguments(ctx, agentrun.ActionProposalValidationInput{
		Run: conversationRun, Bound: bound, Arguments: map[string]interface{}{"reason": "User requested an off-cycle run"},
	})
	if err != nil || !handled || resolved["activationId"] != activation.ID {
		t.Fatalf("resolved exhausted Runbook=%#v handled=%v err=%v", resolved, handled, err)
	}
}

func TestObjectiveConversationReplacesRetiredRunbookScheduleWithFreshActivation(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := portfolio.NewPortfolioService(store).CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Community participation", Goal: "Contribute useful advice", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		ID: "exhausted-hourly-community-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "2.0.19", TriggerID: "hourly", Status: models.RunbookActivationRetired,
		Trigger: runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "review",
			Schedule:  &runbook.Schedule{Cron: "0 43 * * * *", Timezone: "UTC", JitterSeconds: 17, MaximumOccurrences: 7},
			Reporting: &runbook.ReportingPolicy{Channel: "hourly-work", Title: "Hourly work", Milestones: []runbook.ReportingMilestone{runbook.ReportingStarted, runbook.ReportingApprovalRequired, runbook.ReportingCompleted, runbook.ReportingFailed}},
		},
		Input: map[string]interface{}{"communities": []interface{}{"r/saas"}}, Policy: map[string]interface{}{"approvalTimeoutSeconds": float64(900)},
		Budget: &models.BudgetPolicy{MaxTurns: 100, MaxActions: 100}, MaximumConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedTestSpace(t, store, scope)
	conversation, _, err := conversation.NewConversationService(store).CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: objective.Title,
		Origin: &models.ConversationReference{Kind: models.ConversationReferenceObjective, ID: objective.ID, Version: objective.Revision}, IdempotencyKey: "objective-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationRun, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Run hourly for five hours", Source: models.RunSourceChat,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	definition := routine.RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: owner.ID}, Action: definition.Actions[routine.RunbookActionReplaceSchedule]}
	validator, _ := routine.NewRunbookActionValidator(store)
	resolved, handled, err := validator.ResolveActionProposalArguments(ctx, agentrun.ActionProposalValidationInput{
		Run: conversationRun, Bound: bound, Arguments: map[string]interface{}{"maximumOccurrences": float64(5), "reason": "Run five hourly acceptance cycles"},
	})
	if err != nil || !handled || resolved["activationId"] != source.ID {
		t.Fatalf("resolve replacement: resolved=%#v handled=%v err=%v", resolved, handled, err)
	}
	preview, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: conversationRun, Bound: bound, Arguments: resolved})
	if err != nil {
		t.Fatal(err)
	}
	replacementSchedule, _ := preview["replacementSchedule"].(*runbook.Schedule)
	if replacementSchedule == nil || replacementSchedule.MaximumOccurrences != 5 || replacementSchedule.Cron != source.Trigger.Schedule.Cron || preview["sourceActivationId"] != source.ID {
		t.Fatalf("replacement preview = %#v", preview)
	}
	dispatcher, _ := routine.NewRunbookActionDispatcher(store, nil)
	call := &models.ActionCall{ID: "replace-five-hour-schedule", Scope: scope, RunID: conversationRun.ID, DeploymentID: owner.ID}
	result, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: call, Run: conversationRun, Bound: bound, Arguments: resolved})
	if err != nil {
		t.Fatal(err)
	}
	replacement, _ := result["activation"].(*models.RunbookActivation)
	if replacement == nil || replacement.ID == source.ID || replacement.Status != models.RunbookActivationActive || replacement.OccurrencesProcessed != 0 ||
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
	if unchanged == nil || unchanged.Status != models.RunbookActivationRetired || unchanged.Revision != source.Revision || unchanged.Trigger.Schedule.MaximumOccurrences != 7 {
		t.Fatalf("retired source was mutated: %#v", unchanged)
	}
	replayed, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: call, Run: conversationRun, Bound: bound, Arguments: resolved})
	if err != nil || replayed["replayed"] != true || replayed["activation"].(*models.RunbookActivation).ID != replacement.ID {
		t.Fatalf("replacement replay = %#v err=%v", replayed, err)
	}
	delayedCall := &models.ActionCall{ID: "delayed-duplicate-conversation-action", Scope: scope, RunID: conversationRun.ID, DeploymentID: owner.ID}
	delayed, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: delayedCall, Run: conversationRun, Bound: bound, Arguments: resolved})
	if err != nil || delayed["replayed"] != true || delayed["activation"].(*models.RunbookActivation).ID != replacement.ID {
		t.Fatalf("replacement from distinct ActionCall did not converge = %#v err=%v", delayed, err)
	}
	conflicting := maps.Clone(resolved)
	conflicting["maximumOccurrences"] = float64(6)
	if _, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "conflicting-replacement", Scope: scope, RunID: conversationRun.ID, DeploymentID: owner.ID}, Run: conversationRun, Bound: bound, Arguments: conflicting}); !errors.Is(err, models.ErrRunbookActivationIdempotency) {
		t.Fatalf("conflicting replacement error = %v, want %v", err, models.ErrRunbookActivationIdempotency)
	}
	activations, err := store.ListRunbookActivations(ctx, models.RunbookActivationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(activations) != 2 {
		t.Fatalf("replacement lineage contains duplicate successors: activations=%#v err=%v", activations, err)
	}
}

func TestRunbookScheduleReplacementRejectsActiveOrForeignActivation(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	rowan := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	other := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other"}
	objective, err := portfolio.NewPortfolioService(store).CreateObjective(ctx, portfolio.CreateObjectiveRequest{Scope: scope, Owner: other, Title: "Other work", Goal: "Do other work", Status: models.ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		ID: "other-retired-runbook", Scope: scope, Owner: other, ObjectiveID: objective.ID, AssignedAgentID: other.ID,
		DefinitionID: "other", DefinitionVersion: "1", TriggerID: "daily", Status: models.RunbookActivationRetired,
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC", MaximumOccurrences: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := routine.RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: rowan.ID}, Action: definition.Actions[routine.RunbookActionReplaceSchedule]}
	validator, _ := routine.NewRunbookActionValidator(store)
	_, err = validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
		Run: &models.AgentRun{Scope: scope, Owner: rowan, AssignedAgentID: rowan.ID}, Bound: bound,
		Arguments: map[string]interface{}{"activationId": foreign.ID, "maximumOccurrences": float64(5), "reason": "Try foreign work"},
	})
	if err == nil {
		t.Fatal("foreign Runbook schedule replacement was accepted")
	}
}

func TestRunbookStartRejectsActivationOwnedByAnotherAgent(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	rowan := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	other := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other"}
	objective, err := portfolio.NewPortfolioService(store).CreateObjective(ctx, portfolio.CreateObjectiveRequest{Scope: scope, Owner: other, Title: "Other work", Goal: "Do other work", Status: models.ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		ID: "other-runbook", Scope: scope, Owner: other, ObjectiveID: objective.ID, AssignedAgentID: other.ID,
		DefinitionID: "other", DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &models.AgentRun{Scope: scope, Owner: rowan, AssignedAgentID: rowan.ID}
	definition := routine.RunbookManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: rowan.ID}, Action: definition.Actions[routine.RunbookActionStart]}
	validator, _ := routine.NewRunbookActionValidator(store)
	_, err = validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: bound, Arguments: map[string]interface{}{"activationId": activation.ID, "reason": "Try foreign work"},
	})
	if err == nil {
		t.Fatal("foreign Runbook activation was accepted")
	}
}
