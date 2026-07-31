package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

const (
	RunbookManagementSkillID      = "openseal.runbooks"
	RunbookManagementSkillVersion = "1.1.2"
	RunbookActionStart            = "start"
	RunbookActionReplaceSchedule  = "replace_schedule"
	RunbookManagementEndpoint     = "kernel://runbooks"
	runbookActivationResourceType = "runbook_activation"
)

// RunbookManagementSkill exposes operations on reviewed, owner-scoped
// Runbook activations. Starting an activation creates a normal durable Run;
// the model cannot manufacture a definition, execution policy, or authority.
func RunbookManagementSkill() *skill.Definition {
	return &skill.Definition{
		ID: RunbookManagementSkillID, Version: RunbookManagementSkillVersion,
		Name: "Runbooks", Description: "Start reviewed Runbooks or replace an exhausted schedule with a fresh, bounded activation owned by the current Agent or Team.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: RunbookManagementEndpoint},
		Actions: map[string]skill.Action{
			RunbookActionStart: {
				Name:        RunbookActionStart,
				Description: "Start one active, reviewed Runbook now. Omit activationId when the current Objective channel has exactly one active Runbook; otherwise use an activation ID from the conversation context.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"activationId": map[string]interface{}{"type": "string", "minLength": 1},
						"reason":       map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"reason"},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": runbookActivationResourceType},
						"operation":    map[string]interface{}{"type": "string", "const": RunbookActionStart},
						"activation":   map[string]interface{}{"type": "object"},
						"run":          map[string]interface{}{"type": "object"},
						"replayed":     map[string]interface{}{"type": "boolean"},
					},
					"required": []interface{}{"resourceType", "operation", "activation", "run", "replayed"},
				},
			},
			RunbookActionReplaceSchedule: {
				Name:        RunbookActionReplaceSchedule,
				Description: "Replace one retired scheduled Runbook with a fresh reviewed activation. Omit activationId when the current Objective has one Runbook lineage. Only schedule timing changes; the definition, authority, inputs, budget, and reporting remain unchanged.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"anyOf": []interface{}{
						map[string]interface{}{"required": []interface{}{"cron"}},
						map[string]interface{}{"required": []interface{}{"timezone"}},
						map[string]interface{}{"required": []interface{}{"jitterSeconds"}},
						map[string]interface{}{"required": []interface{}{"maximumOccurrences"}},
					},
					"properties": map[string]interface{}{
						"activationId":       map[string]interface{}{"type": "string", "minLength": 1},
						"cron":               map[string]interface{}{"type": "string", "pattern": `^\S+\s+\S+\s+\S+\s+\S+\s+\S+\s+\S+$`, "description": "Optional six-field cron expression including seconds, for example 0 0 * * * *; omit to preserve the reviewed cadence."},
						"timezone":           map[string]interface{}{"type": "string", "minLength": 1, "description": "Optional IANA timezone; omit to preserve the reviewed timezone."},
						"jitterSeconds":      map[string]interface{}{"type": "integer", "minimum": 0, "maximum": 2678400, "description": "Optional timing variation; omit to preserve the reviewed value."},
						"maximumOccurrences": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 1000000, "description": "Optional bounded number of occurrences; omit to preserve the reviewed value."},
						"reason":             map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"reason"},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType":     map[string]interface{}{"type": "string", "const": runbookActivationResourceType},
						"operation":        map[string]interface{}{"type": "string", "const": RunbookActionReplaceSchedule},
						"sourceActivation": map[string]interface{}{"type": "object"},
						"activation":       map[string]interface{}{"type": "object"},
						"replayed":         map[string]interface{}{"type": "boolean"},
					},
					"required": []interface{}{"resourceType", "operation", "sourceActivation", "activation", "replayed"},
				},
			},
		},
	}
}

type runbookStartArguments struct {
	ActivationID string `json:"activationId"`
	Reason       string `json:"reason"`
}

type runbookReplaceScheduleArguments struct {
	ActivationID       string `json:"activationId"`
	Cron               string `json:"cron"`
	Timezone           string `json:"timezone"`
	JitterSeconds      *int64 `json:"jitterSeconds"`
	MaximumOccurrences *int64 `json:"maximumOccurrences"`
	Reason             string `json:"reason"`
}

type runbookActionStore interface {
	KernelStore
	ConversationStore
}

// RunbookActionValidator resolves an omitted activation from the current
// Objective channel, then proves that the activation is active and belongs to
// the conversation owner before policy evaluation or approval persistence.
type RunbookActionValidator struct{ store runbookActionStore }

func NewRunbookActionValidator(store runbookActionStore) (*RunbookActionValidator, error) {
	if store == nil {
		return nil, errors.New("Runbook action store is required")
	}
	return &RunbookActionValidator{store: store}, nil
}

func (v *RunbookActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isRunbookAction(input.Bound) {
		return nil, false, nil
	}
	arguments := cloneMap(input.Arguments)
	if id, _ := arguments["activationId"].(string); strings.TrimSpace(id) != "" {
		return arguments, true, nil
	}
	if v == nil || v.store == nil || input.Run == nil {
		return nil, true, errors.New("Runbook action validator is not configured")
	}
	objectiveID, err := conversationObjectiveOrigin(ctx, v.store, input.Run)
	if err != nil {
		return nil, true, err
	}
	statuses := []RunbookActivationStatus{RunbookActivationActive}
	if input.Bound.Action.Name == RunbookActionReplaceSchedule {
		statuses = []RunbookActivationStatus{RunbookActivationRetired}
	}
	filter := RunbookActivationFilter{Scope: input.Run.Scope, Owner: &input.Run.Owner, ObjectiveID: objectiveID, Statuses: statuses, Limit: 100}
	if input.Bound.Action.Name == RunbookActionReplaceSchedule {
		filter.TriggerKinds = []runbook.TriggerKind{runbook.TriggerSchedule}
	}
	activations, err := v.store.ListRunbookActivations(ctx, filter)
	if err != nil {
		return nil, true, err
	}
	if len(activations) == 0 {
		if input.Bound.Action.Name == RunbookActionReplaceSchedule {
			return nil, true, errors.New("no retired scheduled Runbook is available in this conversation context")
		}
		return nil, true, errors.New("no active reviewed Runbook is available in this conversation context")
	}
	if input.Bound.Action.Name == RunbookActionReplaceSchedule {
		selected, selectErr := selectLatestRunbookLineage(activations)
		if selectErr != nil {
			return nil, true, selectErr
		}
		arguments["activationId"] = selected.ID
		return arguments, true, nil
	}
	if len(activations) != 1 {
		return nil, true, errors.New("multiple active Runbooks are available; choose one by activationId")
	}
	arguments["activationId"] = activations[0].ID
	return arguments, true, nil
}

func (v *RunbookActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isRunbookAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.store == nil || input.Run == nil || input.Bound.Binding == nil {
		return nil, errors.New("Runbook action validator is not configured")
	}
	switch input.Bound.Action.Name {
	case RunbookActionStart:
		args, activation, objective, err := resolveRunbookStart(ctx, v.store, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"resourceType": runbookActivationResourceType, "operation": RunbookActionStart,
			"activationId": activation.ID, "objectiveId": objective.ID,
			"definitionId": activation.DefinitionID, "definitionVersion": activation.DefinitionVersion,
			"entrypoint": activation.Trigger.Entrypoint, "reason": args.Reason,
		}, nil
	case RunbookActionReplaceSchedule:
		args, activation, objective, schedule, err := resolveRunbookScheduleReplacement(ctx, v.store, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"resourceType": runbookActivationResourceType, "operation": RunbookActionReplaceSchedule,
			"sourceActivationId": activation.ID, "objectiveId": objective.ID,
			"definitionId": activation.DefinitionID, "definitionVersion": activation.DefinitionVersion,
			"entrypoint": activation.Trigger.Entrypoint, "reason": args.Reason,
			"currentSchedule": activation.Trigger.Schedule, "replacementSchedule": schedule,
		}, nil
	default:
		return nil, errors.New("unsupported Runbook action")
	}
}

type RunbookActionDispatcher struct {
	store    runbookActionStore
	fallback ActionDispatcher
}

func NewRunbookActionDispatcher(store runbookActionStore, fallback ActionDispatcher) (*RunbookActionDispatcher, error) {
	if store == nil {
		return nil, errors.New("Runbook action store is required")
	}
	return &RunbookActionDispatcher{store: store, fallback: fallback}, nil
}

func (d *RunbookActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isRunbookAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || input.Call == nil || input.Run == nil {
		return nil, errors.New("Runbook action dispatcher is not configured")
	}
	if input.Bound.Action.Name == RunbookActionReplaceSchedule {
		_, source, _, schedule, err := resolveRunbookScheduleReplacement(ctx, d.store, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		trigger := source.Trigger
		trigger.Schedule = schedule
		idempotencyKey := "conversation-runbook-replace-schedule:" + input.Call.ID
		activationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(source.Scope.Kind+"\x00"+source.Scope.ID+"\x00runbook-activation\x00"+hashString(idempotencyKey))).String()
		existing, err := d.store.GetRunbookActivation(ctx, source.Scope, activationID)
		if err != nil {
			return nil, err
		}
		activation, err := NewRunbookActivationService(d.store).Create(ctx, CreateRunbookActivationRequest{
			Scope: source.Scope, Owner: source.Owner, ObjectiveID: source.ObjectiveID, AssignedAgentID: source.AssignedAgentID,
			DefinitionID: source.DefinitionID, DefinitionVersion: source.DefinitionVersion, TriggerID: source.TriggerID,
			Trigger: trigger, Input: source.Input, Policy: source.Policy, Budget: source.Budget,
			MaximumConcurrent: source.MaximumConcurrent, Status: RunbookActivationActive,
			IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"resourceType": runbookActivationResourceType, "operation": RunbookActionReplaceSchedule,
			"sourceActivation": source, "activation": activation,
			"replayed": existing != nil,
		}, nil
	}
	_, activation, _, err := resolveRunbookStart(ctx, d.store, input.Run, input.Arguments)
	if err != nil {
		return nil, err
	}
	visibility := ActivityVisibilityPrivate
	if input.Run.Owner.Type == OwnerTypeTeam {
		visibility = ActivityVisibilityTeam
	}
	result, err := StartRunbookActivation(ctx, d.store, input.Run.Scope, activation.ID, StartRunbookActivationRequest{
		IdempotencyKey: "conversation-runbook-start:" + input.Call.ID,
		Actor:          ActivityActor{Type: "agent", ID: activation.AssignedAgentID},
		Visibility:     visibility,
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"resourceType": runbookActivationResourceType, "operation": RunbookActionStart,
		"activation": activation, "run": result.Run, "replayed": result.Event == nil,
	}, nil
}

func resolveRunbookScheduleReplacement(ctx context.Context, store runbookActionStore, run *AgentRun, arguments map[string]interface{}) (runbookReplaceScheduleArguments, *RunbookActivation, *Objective, *runbook.Schedule, error) {
	var args runbookReplaceScheduleArguments
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return args, nil, nil, nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return args, nil, nil, nil, fmt.Errorf("decode Runbook schedule replacement arguments: %w", err)
	}
	args.ActivationID, args.Cron, args.Timezone, args.Reason = strings.TrimSpace(args.ActivationID), strings.TrimSpace(args.Cron), strings.TrimSpace(args.Timezone), strings.TrimSpace(args.Reason)
	if args.ActivationID == "" || args.Reason == "" {
		return args, nil, nil, nil, errors.New("Runbook schedule replacement requires activationId and reason")
	}
	if args.Cron == "" && args.Timezone == "" && args.JitterSeconds == nil && args.MaximumOccurrences == nil {
		return args, nil, nil, nil, errors.New("Runbook schedule replacement requires at least one timing change")
	}
	activation, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
	if err != nil {
		return args, nil, nil, nil, err
	}
	if activation == nil {
		return args, nil, nil, nil, ErrRunbookActivationNotFound
	}
	if activation.Owner != run.Owner {
		return args, nil, nil, nil, errors.New("Runbook activation is not owned by the conversation Agent or Team")
	}
	if activation.Status != RunbookActivationRetired || activation.Trigger.Kind != runbook.TriggerSchedule || activation.Trigger.Schedule == nil {
		return args, nil, nil, nil, errors.New("Runbook schedule replacement requires a retired scheduled activation")
	}
	objective, err := store.GetObjective(ctx, run.Scope, activation.ObjectiveID)
	if err != nil {
		return args, nil, nil, nil, err
	}
	if objective == nil {
		return args, nil, nil, nil, ErrObjectiveNotFound
	}
	if objective.Owner != run.Owner || objective.Status != ObjectiveStatusActive {
		return args, nil, nil, nil, errors.New("Runbook schedule replacement requires its active owning Objective")
	}
	schedule := *activation.Trigger.Schedule
	if args.Cron != "" {
		schedule.Cron = args.Cron
	}
	if args.Timezone != "" {
		schedule.Timezone = args.Timezone
	}
	if args.JitterSeconds != nil {
		schedule.JitterSeconds = *args.JitterSeconds
	}
	if args.MaximumOccurrences != nil {
		schedule.MaximumOccurrences = *args.MaximumOccurrences
	}
	if err := schedule.Validate(); err != nil {
		return args, nil, nil, nil, fmt.Errorf("replacement schedule: %w", err)
	}
	return args, activation, objective, &schedule, nil
}

func selectLatestRunbookLineage(activations []*RunbookActivation) (*RunbookActivation, error) {
	if len(activations) == 0 {
		return nil, ErrRunbookActivationNotFound
	}
	definitionID, triggerID := activations[0].DefinitionID, activations[0].TriggerID
	selected := activations[0]
	for _, activation := range activations[1:] {
		if activation.DefinitionID != definitionID || activation.TriggerID != triggerID {
			return nil, errors.New("multiple retired Runbook lineages are available; choose one by activationId")
		}
		if activation.UpdatedAt.After(selected.UpdatedAt) || activation.UpdatedAt.Equal(selected.UpdatedAt) && activation.ID > selected.ID {
			selected = activation
		}
	}
	return selected, nil
}

func resolveRunbookStart(ctx context.Context, store runbookActionStore, run *AgentRun, arguments map[string]interface{}) (runbookStartArguments, *RunbookActivation, *Objective, error) {
	var args runbookStartArguments
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return args, nil, nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return args, nil, nil, fmt.Errorf("decode Runbook action arguments: %w", err)
	}
	args.ActivationID, args.Reason = strings.TrimSpace(args.ActivationID), strings.TrimSpace(args.Reason)
	if args.ActivationID == "" || args.Reason == "" {
		return args, nil, nil, errors.New("Runbook start requires activationId and reason")
	}
	activation, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
	if err != nil {
		return args, nil, nil, err
	}
	if activation == nil {
		return args, nil, nil, ErrRunbookActivationNotFound
	}
	if activation.Owner != run.Owner {
		return args, nil, nil, errors.New("Runbook activation is not owned by the conversation Agent or Team")
	}
	if activation.Status != RunbookActivationActive {
		return args, nil, nil, ErrRunbookActivationInactive
	}
	objective, err := store.GetObjective(ctx, run.Scope, activation.ObjectiveID)
	if err != nil {
		return args, nil, nil, err
	}
	if objective == nil {
		return args, nil, nil, ErrObjectiveNotFound
	}
	if objective.Owner != run.Owner || objective.Status != ObjectiveStatusActive {
		return args, nil, nil, errors.New("Runbook start requires its active owning Objective")
	}
	return args, activation, objective, nil
}

func conversationObjectiveOrigin(ctx context.Context, store ConversationStore, run *AgentRun) (string, error) {
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	if strings.TrimSpace(conversationID) == "" {
		return "", nil
	}
	conversation, err := store.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return "", err
	}
	if conversation.Origin != nil && conversation.Origin.Kind == ConversationReferenceObjective {
		return conversation.Origin.ID, nil
	}
	return "", nil
}

func isRunbookAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil && bound.Definition.ID == RunbookManagementSkillID &&
		bound.Definition.Version == RunbookManagementSkillVersion &&
		(bound.Action.Name == RunbookActionStart || bound.Action.Name == RunbookActionReplaceSchedule)
}
