package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Test-only access to routine internals for the external routine scenario
// tests, which use fixtures shared with tests of other packages and so
// cannot be in-package tests.

const MaximumRoutineTestEventPayload = maximumRoutineTestEventPayload

type (
	RoutineEventActions = routineEventActions
	RunbookActionStore  = runbookActionStore
)

func RoutineEventIdempotencyKey(batchID string) string {
	return routineEventIdempotencyKey(batchID)
}

func StartActivationOccurrence(ctx context.Context, store models.RunCommandStore, reportingStore models.ConversationStore, activation *models.RunbookActivation, objective *models.Objective, source models.RunSource, idempotencyKey string, extraContext map[string]interface{}, actor models.ActivityActor, visibility models.ActivityVisibility, preparer models.AcceptedRunExecutionPreparer) (*agentrun.AgentRunCommandResult, error) {
	return startActivationOccurrence(ctx, store, reportingStore, activation, objective, source, idempotencyKey, extraContext, actor, visibility, preparer)
}

func ResolveRunbookScheduleReplacement(ctx context.Context, store runbookActionStore, run *models.AgentRun, arguments map[string]interface{}) (runbookReplaceScheduleArguments, *models.RunbookActivation, *models.Objective, *runbook.Schedule, error) {
	return resolveRunbookScheduleReplacement(ctx, store, run, arguments)
}

func RoutineEventPreview(trigger *models.RoutineEventTrigger) map[string]interface{} {
	return routineEventPreview(trigger)
}

func ResolveScheduledTaskCreate(ctx context.Context, store runbookActionStore, files ChatFileResolver, events routineEventActions, run *models.AgentRun, activationID string, args map[string]interface{}) (*scheduledTaskCreateArguments, *models.ScheduledAgentTask, *runbook.Schedule, error) {
	return resolveScheduledTaskCreate(ctx, store, files, events, run, activationID, args)
}

func ActivationGoal(a *models.RunbookActivation, o *models.Objective) string {
	return activationGoal(a, o)
}
