package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	RunbookActionCreateTask = "create_task"
	RunbookActionList       = "list"
	RunbookActionSetStatus  = "set_status"
	scheduledTaskContextKey = "scheduledAgentTask"
)

// ScheduledAgentTask is the cognitive execution method of an Objective-owned
// routine. It pins work and its reporting destination, never credentials or
// model-generated execution authority. Every occurrence resolves the current
// deployed Agent and its currently authorized tools through the normal worker.
type ScheduledAgentTask struct {
	Goal            string `json:"goal"`
	ConversationID  string `json:"conversationId"`
	SourceMessageID string `json:"sourceMessageId"`
}

func (t *ScheduledAgentTask) Validate() error {
	if t == nil || strings.TrimSpace(t.Goal) == "" || len(t.Goal) > 16000 || !validOpaqueIdentifier(t.ConversationID, 256) || !validOpaqueIdentifier(t.SourceMessageID, 256) {
		return errors.New("scheduled task requires bounded work and an originating conversation message")
	}
	return authoring.ValidateAuthoringPrompt(t.Goal)
}
func cloneScheduledAgentTask(t *ScheduledAgentTask) *ScheduledAgentTask {
	if t == nil {
		return nil
	}
	copy := *t
	return &copy
}
func activationGoal(a *RunbookActivation, o *Objective) string {
	if a.Task != nil {
		return a.Task.Goal
	}
	return o.Goal
}

func scheduledTaskConversation(ctx context.Context, store ConversationStore, scope Scope, owner ObjectiveOwner, task *ScheduledAgentTask) (*Conversation, error) {
	if err := task.Validate(); err != nil {
		return nil, err
	}
	c, err := store.GetConversation(ctx, scope, task.ConversationID)
	if err != nil {
		return nil, err
	}
	if c == nil || c.Scope != scope || c.Owner != owner || c.Status != ConversationStatusActive {
		return nil, errors.New("scheduled task reporting conversation is unavailable")
	}
	m, err := store.GetChannelMessage(ctx, scope, c.ID, task.SourceMessageID)
	if err != nil {
		return nil, err
	}
	if m == nil || m.Scope != scope || m.ConversationID != c.ID || m.Sender.Type != ConversationParticipantUser || m.Audience.Kind != ConversationAudienceChannel {
		return nil, errors.New("scheduled tasks require a user request visible to the reporting channel")
	}
	return c, nil
}

func validateScheduledTaskTarget(ctx context.Context, reader RunbookActivationReader, catalog RunbookDefinitionCatalog, a *RunbookActivation) error {
	if err := a.Validate(); err != nil {
		return err
	}
	d, err := catalog.GetDeployment(ctx, capability.ScopeReference{Kind: a.Scope.Kind, ID: a.Scope.ID}, a.AssignedAgentID)
	if err != nil {
		return fmt.Errorf("%w: assigned Agent is unavailable", ErrRunbookActivationUnverified)
	}
	if d == nil || d.Scope.Kind != a.Scope.Kind || d.Scope.ID != a.Scope.ID || d.RolloutStatus != kernelagent.RolloutActive {
		return fmt.Errorf("%w: assigned Agent is inactive", ErrRunbookActivationUnverified)
	}
	store, ok := reader.(ConversationStore)
	if !ok {
		return fmt.Errorf("%w: reporting store unavailable", ErrRunbookActivationUnverified)
	}
	if _, err := scheduledTaskConversation(ctx, store, a.Scope, a.Owner, a.Task); err != nil {
		return fmt.Errorf("%w: %v", ErrRunbookActivationUnverified, err)
	}
	return nil
}

func prepareActivationReporting(ctx context.Context, store ConversationStore, a *RunbookActivation, runID string, values map[string]interface{}) (*Conversation, string, error) {
	if a.Task == nil {
		return prepareRunReporting(ctx, store, a.Scope, a.Owner, a.Trigger.Reporting, runID, values)
	}
	if store == nil {
		return nil, "", errors.New("scheduled task reporting store unavailable")
	}
	c, err := scheduledTaskConversation(ctx, store, a.Scope, a.Owner, a.Task)
	if err != nil {
		return nil, "", err
	}
	values[conversationRunContextConversationID] = c.ID
	values[conversationRunContextTriggerID] = a.Task.SourceMessageID
	values[runReportingContextRootRunID] = runID
	values[runReportingContextMilestones] = []interface{}{string(runbook.ReportingCompleted), string(runbook.ReportingFailed)}
	values[scheduledTaskContextKey] = true
	return c, "", nil
}

func scheduledTaskCreateAction() skill.Action {
	return skill.Action{Name: RunbookActionCreateTask, Description: "Create and activate a recurring scheduled task when the user requests future or repeated work. The current Agent executes the supplied goal with its authorized tools on each occurrence and posts results to this chat. Use a six-field cron including seconds and an explicit IANA timezone; clarify ambiguous timing. This creates a real routine, not a sleeping chat reply. Tool access and approval requirements still apply on each run.", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"title", "goal", "cron", "timezone"}, "properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 240}, "goal": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 16000, "description": "Self-contained work to perform on every occurrence, including the source to use and requested result. Do not include credentials or request another schedule."},
			"cron": map[string]interface{}{"type": "string", "pattern": `^\S+\s+\S+\s+\S+\s+\S+\s+\S+\s+\S+$`, "description": "Six-field cron: seconds minutes hours day-of-month month weekday."}, "timezone": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 100}, "maximumOccurrences": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 1000000, "description": "Omit for an ongoing routine; supply only when the user requested a bounded number of executions."},
		}}, OutputSchema: scheduledTaskResultSchema(RunbookActionCreateTask)}
}
func scheduledTaskListAction() skill.Action {
	return skill.Action{Name: RunbookActionList, Description: "List routines owned by this Agent or Team, including exact activation IDs, schedules, status, revisions, next occurrence and originating chat. Use these IDs to start, pause, resume, cancel or replace a schedule.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"offset": map[string]interface{}{"type": "integer", "minimum": 0}}}, OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"activations": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}}, "nextOffset": map[string]interface{}{"type": "integer"}}, "required": []interface{}{"activations"}}}
}
func scheduledTaskStatusAction() skill.Action {
	return skill.Action{Name: RunbookActionSetStatus, Description: "Pause, resume or cancel future occurrences of an owned routine using its exact activationId and expectedRevision from list. active resumes; paused suspends; retired cancels permanently. Existing runs continue separately and can be controlled with Run tools. To change timing, retire then replace_schedule; the work and reporting destination remain unchanged.", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired, InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"activationId", "expectedRevision", "status", "reason"}, "properties": map[string]interface{}{"activationId": map[string]interface{}{"type": "string", "minLength": 1}, "expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1}, "status": map[string]interface{}{"type": "string", "enum": []interface{}{"active", "paused", "retired"}}, "reason": map[string]interface{}{"type": "string", "minLength": 1}}}, OutputSchema: scheduledTaskResultSchema(RunbookActionSetStatus)}
}
func scheduledTaskResultSchema(operation string) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"resourceType": map[string]interface{}{"type": "string", "const": runbookActivationResourceType}, "operation": map[string]interface{}{"type": "string", "const": operation}, "activation": map[string]interface{}{"type": "object"}, "objective": map[string]interface{}{"type": "object"}, "replayed": map[string]interface{}{"type": "boolean"}}, "required": []interface{}{"resourceType", "operation", "activation", "replayed"}}
}

type scheduledTaskCreateArguments struct {
	Title              string `json:"title"`
	Goal               string `json:"goal"`
	Cron               string `json:"cron"`
	Timezone           string `json:"timezone"`
	MaximumOccurrences int64  `json:"maximumOccurrences,omitempty"`
}
type scheduledTaskStatusArguments struct {
	ActivationID     string                  `json:"activationId"`
	ExpectedRevision int64                   `json:"expectedRevision"`
	Status           RunbookActivationStatus `json:"status"`
	Reason           string                  `json:"reason"`
}

func decodeScheduledTaskArguments(arguments map[string]interface{}, target interface{}) error {
	b, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

func resolveScheduledTaskCreate(ctx context.Context, store runbookActionStore, run *AgentRun, args map[string]interface{}) (*scheduledTaskCreateArguments, *ScheduledAgentTask, *runbook.Schedule, error) {
	var a scheduledTaskCreateArguments
	if err := decodeScheduledTaskArguments(args, &a); err != nil {
		return nil, nil, nil, err
	}
	a.Title, a.Goal, a.Cron, a.Timezone = strings.TrimSpace(a.Title), strings.TrimSpace(a.Goal), strings.TrimSpace(a.Cron), strings.TrimSpace(a.Timezone)
	if a.Title == "" || len(a.Title) > 240 || a.MaximumOccurrences < 0 || a.MaximumOccurrences > 1000000 {
		return nil, nil, nil, errors.New("scheduled task title or occurrence limit is invalid")
	}
	if err := authoring.ValidateAuthoringPrompt(a.Title); err != nil {
		return nil, nil, nil, err
	}
	if run == nil || run.Kind != RunKindConversation || run.AssignedAgentID == "" || (run.Owner.Type == OwnerTypeAgent && run.Owner.ID != run.AssignedAgentID) {
		return nil, nil, nil, errors.New("scheduled task creation requires its owning Agent's user conversation")
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	messageID, _ := run.Context[conversationRunContextTriggerID].(string)
	task := &ScheduledAgentTask{Goal: a.Goal, ConversationID: conversationID, SourceMessageID: messageID}
	if _, err := scheduledTaskConversation(ctx, store, run.Scope, run.Owner, task); err != nil {
		return nil, nil, nil, err
	}
	schedule := &runbook.Schedule{Cron: a.Cron, Timezone: a.Timezone, MaximumOccurrences: a.MaximumOccurrences}
	if err := schedule.Validate(); err != nil {
		return nil, nil, nil, err
	}
	return &a, task, schedule, nil
}
func resolveOwnedTaskStatus(ctx context.Context, store runbookActionStore, run *AgentRun, arguments map[string]interface{}) (*scheduledTaskStatusArguments, *RunbookActivation, error) {
	var args scheduledTaskStatusArguments
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(args.Reason) == "" || args.ActivationID == "" || args.ExpectedRevision < 1 {
		return nil, nil, errors.New("routine status change requires an ID, revision and reason")
	}
	if args.Status != RunbookActivationActive && args.Status != RunbookActivationPaused && args.Status != RunbookActivationRetired {
		return nil, nil, errors.New("unsupported routine status")
	}
	a, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
	if err != nil {
		return nil, nil, err
	}
	if a == nil || a.Owner != run.Owner {
		return nil, nil, errors.New("routine is not owned by this Agent")
	}
	if a.Revision != args.ExpectedRevision {
		return nil, nil, ErrRunbookActivationRevision
	}
	if a.Status == RunbookActivationRetired && args.Status != RunbookActivationRetired {
		return nil, nil, errors.New("cancelled routine cannot be resumed; replace its schedule")
	}
	return &args, a, nil
}
func validateScheduledTaskAction(ctx context.Context, store runbookActionStore, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if input.Bound.Binding.DeploymentID != input.Run.Owner.ID {
		return nil, errors.New("routine binding targets another Agent")
	}
	switch input.Bound.Action.Name {
	case RunbookActionCreateTask:
		args, task, schedule, err := resolveScheduledTaskCreate(ctx, store, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionCreateTask, "title": args.Title, "goal": task.Goal, "schedule": schedule, "conversationId": task.ConversationID}, nil
	case RunbookActionList:
		return nil, nil
	case RunbookActionSetStatus:
		args, a, err := resolveOwnedTaskStatus(ctx, store, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetStatus, "activationId": a.ID, "currentStatus": a.Status, "status": args.Status, "reason": args.Reason}, nil
	}
	return nil, errors.New("unsupported scheduled task action")
}
func dispatchScheduledTaskAction(ctx context.Context, store runbookActionStore, input ActionDispatchInput) (map[string]interface{}, error) {
	run, err := store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil {
		return nil, err
	}
	if input.Call.ID == "" || run == nil || run.Scope != input.Run.Scope || run.ID != input.Run.ID || input.Call.DeploymentID != run.Owner.ID || input.Bound.Binding.DeploymentID != run.Owner.ID {
		return nil, errors.New("routine execution does not match its persisted Agent run")
	}
	switch input.Bound.Action.Name {
	case RunbookActionList:
		var args struct {
			Offset int `json:"offset"`
		}
		if err := decodeScheduledTaskArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		if args.Offset < 0 {
			return nil, errors.New("invalid routine offset")
		}
		values, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: run.Scope, Owner: &run.Owner, Limit: 50, Offset: args.Offset})
		if err != nil {
			return nil, err
		}
		result := map[string]interface{}{"activations": values}
		if len(values) == 50 {
			result["nextOffset"] = args.Offset + 50
		}
		return result, nil
	case RunbookActionCreateTask:
		args, task, schedule, err := resolveScheduledTaskCreate(ctx, store, run, input.Arguments)
		if err != nil {
			return nil, err
		}
		if input.Call.IdempotencyKey == "" {
			return nil, errors.New("scheduled task requires action idempotency")
		}
		objective, err := NewPortfolioService(store).CreateObjectiveIdempotent(ctx, CreateObjectiveRequest{Scope: run.Scope, Owner: run.Owner, Title: args.Title, Goal: task.Goal, Status: ObjectiveStatusActive, IdempotencyKey: input.Call.IdempotencyKey, ExecutionPolicy: &ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1}, Actor: ActivityActor{Type: "agent", ID: run.AssignedAgentID}, Visibility: ActivityVisibilityScope})
		if err != nil {
			return nil, err
		}
		activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{Scope: run.Scope, Owner: run.Owner, ObjectiveID: objective.Objective.ID, AssignedAgentID: run.AssignedAgentID, Task: task, TriggerID: "schedule", Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: schedule}, MaximumConcurrent: 1, IdempotencyKey: input.Call.IdempotencyKey})
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionCreateTask, "activation": activation, "objective": objective.Objective, "replayed": !objective.Created}, nil
	case RunbookActionSetStatus:
		// Replay after the lifecycle update committed but the action receipt did not.
		var args scheduledTaskStatusArguments
		if err := decodeScheduledTaskArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		current, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
		if err != nil {
			return nil, err
		}
		if current == nil || current.Owner != run.Owner {
			return nil, errors.New("routine is not owned by this Agent")
		}
		// The action checkpoint uses CAS; arbitrary stale revisions remain conflicts.
		if current.LifecycleActionID == input.Call.ID && current.Status == args.Status {
			return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetStatus, "activation": current, "replayed": true}, nil
		}
		if _, _, err := resolveOwnedTaskStatus(ctx, store, run, input.Arguments); err != nil {
			return nil, err
		}
		activation, err := NewRunbookActivationService(store).Update(ctx, run.Scope, current.ID, UpdateRunbookActivationRequest{ExpectedRevision: args.ExpectedRevision, Status: args.Status, ActionID: input.Call.ID})
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetStatus, "activation": activation, "replayed": false}, nil
	}
	return nil, errors.New("unsupported scheduled task action")
}

func scheduledTaskConversationCompletion(run *AgentRun, result map[string]interface{}) (*governedConversationCompletion, bool) {
	operation := conversationResultString(result, "operation")
	if operation != RunbookActionCreateTask && operation != RunbookActionSetStatus {
		return nil, false
	}
	activation := conversationResultMap(result["activation"])
	id := conversationResultString(activation, "id")
	if !validOpaqueIdentifier(id, 256) {
		return nil, false
	}
	content := "The scheduled task is active. Results will appear in the chat where it was created."
	if operation == RunbookActionSetStatus {
		status := conversationResultString(activation, "status")
		if status == string(RunbookActivationRetired) {
			status = "cancelled"
		}
		content = "The routine is now " + status + "."
	}
	if operation == RunbookActionCreateTask {
		if due, err := time.Parse(time.RFC3339Nano, conversationResultString(activation, "nextRunAt")); err == nil {
			trigger := conversationResultMap(activation["trigger"])
			schedule := conversationResultMap(trigger["schedule"])
			zone := conversationResultString(schedule, "timezone")
			if location, err := time.LoadLocation(zone); err == nil {
				due = due.In(location)
			}
			content += " Next run: " + due.Format("2 Jan, 3:04 PM") + " (" + zone + ")."
		}
	}
	return &governedConversationCompletion{Content: content, ResourceType: runbookActivationResourceType, ResourceID: id, References: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}}, true
}
