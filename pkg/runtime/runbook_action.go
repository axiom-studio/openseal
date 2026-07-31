package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	RunbookManagementSkillID      = "openseal.runbooks"
	RunbookManagementSkillVersion = "1.0.0"
	RunbookActionStart            = "start"
	RunbookManagementEndpoint     = "kernel://runbooks"
	runbookActivationResourceType = "runbook_activation"
)

// RunbookManagementSkill exposes operations on reviewed, owner-scoped
// Runbook activations. Starting an activation creates a normal durable Run;
// the model cannot manufacture a definition, execution policy, or authority.
func RunbookManagementSkill() *skill.Definition {
	return &skill.Definition{
		ID: RunbookManagementSkillID, Version: RunbookManagementSkillVersion,
		Name: "Runbooks", Description: "Start reviewed Runbooks owned by the current Agent or Team.",
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
		},
	}
}

type runbookStartArguments struct {
	ActivationID string `json:"activationId"`
	Reason       string `json:"reason"`
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
	filter := RunbookActivationFilter{Scope: input.Run.Scope, Owner: &input.Run.Owner, ObjectiveID: objectiveID, Statuses: []RunbookActivationStatus{RunbookActivationActive}, Limit: 2}
	activations, err := v.store.ListRunbookActivations(ctx, filter)
	if err != nil {
		return nil, true, err
	}
	if len(activations) == 0 {
		return nil, true, errors.New("no active reviewed Runbook is available in this conversation context")
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
		bound.Definition.Version == RunbookManagementSkillVersion && bound.Action.Name == RunbookActionStart
}
