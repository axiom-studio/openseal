package agentrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	RunManagementSkillVersion = "1.0.0"
	RunManagementEndpoint     = "kernel://runs"
	runResourceType           = "agent_run"
)

// RunManagementSkill exposes lifecycle controls for non-terminal Runs owned by
// the Agent or Team handling the current conversation. The model selects only
// from Runs and controls projected by the kernel; ownership and revisions are
// re-resolved before execution.
func RunManagementSkill() *skill.Definition {
	actions := make(map[string]skill.Action, 3)
	for _, name := range []string{models.RunActionPause, models.RunActionResume, models.RunActionCancel} {
		actions[name] = skill.Action{
			Name:        name,
			Description: runActionDescription(name),
			Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
			Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"runId":            map[string]interface{}{"type": "string", "minLength": 1},
					"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
					"reason":           map[string]interface{}{"type": "string", "minLength": 1},
				},
				"required": []interface{}{"runId", "reason"},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"resourceType": map[string]interface{}{"type": "string", "const": runResourceType},
					"operation":    map[string]interface{}{"type": "string", "const": name},
					"run":          map[string]interface{}{"type": "object"},
					"event":        map[string]interface{}{"type": "object"},
				},
				"required": []interface{}{"resourceType", "operation", "run"},
			},
		}
	}
	return &skill.Definition{
		ID: models.RunManagementSkillID, Version: RunManagementSkillVersion,
		Name: "Runs", Description: "Pause, resume, or cancel current work owned by this Agent or Team.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: RunManagementEndpoint}, Actions: actions,
	}
}

func runActionDescription(action string) string {
	switch action {
	case models.RunActionPause:
		return "Pause an active Run shown in the current conversation context."
	case models.RunActionResume:
		return "Resume a paused Run shown in the current conversation context."
	default:
		return "Cancel a non-terminal Run shown in the current conversation context, including its pending approval when applicable."
	}
}

type RunActionValidator struct{ store models.RunCommandStore }

func NewRunActionValidator(store models.RunCommandStore) (*RunActionValidator, error) {
	if store == nil {
		return nil, errors.New("Run command store is required")
	}
	return &RunActionValidator{store: store}, nil
}

func (v *RunActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isRunManagementAction(input.Bound) {
		return nil, false, nil
	}
	arguments := maps.Clone(input.Arguments)
	if _, supplied := arguments["expectedRevision"]; supplied {
		return arguments, true, nil
	}
	runID := strings.TrimSpace(fmt.Sprint(arguments["runId"]))
	if runID == "" || input.Run == nil {
		return arguments, true, nil
	}
	target, err := v.store.GetAgentRun(ctx, input.Run.Scope, runID)
	if err != nil {
		return nil, true, err
	}
	if target == nil {
		return nil, true, models.ErrRunNotFound
	}
	arguments["expectedRevision"] = target.Revision
	return arguments, true, nil
}

func (v *RunActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isRunManagementAction(input.Bound) {
		return nil, nil
	}
	if input.Run == nil {
		return nil, errors.New("conversation Run is required")
	}
	runID := strings.TrimSpace(fmt.Sprint(input.Arguments["runId"]))
	revision, ok := runControlPositiveInt64(input.Arguments["expectedRevision"])
	if runID == "" || !ok {
		return nil, errors.New("Run control requires a current Run and revision")
	}
	target, err := v.store.GetAgentRun(ctx, input.Run.Scope, runID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, models.ErrRunNotFound
	}
	if target.Owner != input.Run.Owner {
		return nil, errors.New("Run is not owned by the conversation Agent or Team")
	}
	if err := validateConversationTaskControl(ctx, v.store, input.Run, target); err != nil {
		return nil, err
	}
	if target.ID == input.Run.ID || target.Kind == models.RunKindConversation {
		return nil, errors.New("a conversation cannot control its own coordination Run")
	}
	if target.Revision != revision {
		return nil, models.ErrRevisionConflict
	}
	if !slices.Contains(ApplicableAgentRunCommands(target), input.Bound.Action.Name) {
		return nil, fmt.Errorf("%w: cannot %s %s Run", models.ErrInvalidRunTransition, input.Bound.Action.Name, target.Status)
	}
	return map[string]interface{}{
		"resourceType": runResourceType, "operation": input.Bound.Action.Name,
		"runId": target.ID, "expectedRevision": target.Revision,
		"current": map[string]interface{}{"status": target.Status, "goal": target.Goal, "source": target.Source},
	}, nil
}

type RunActionDispatcher struct {
	commands *RunCommandService
	store    models.RunCommandStore
	fallback ActionDispatcher
}

func NewRunActionDispatcher(store models.RunCommandStore, fallback ActionDispatcher) (*RunActionDispatcher, error) {
	if store == nil {
		return nil, errors.New("Run command store is required")
	}
	return &RunActionDispatcher{commands: NewRunCommandService(store), store: store, fallback: fallback}, nil
}

func (d *RunActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isRunManagementAction(input.Bound) {
		if d.fallback == nil {
			return nil, errors.New("action dispatcher is not configured")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	runID := strings.TrimSpace(fmt.Sprint(input.Call.Arguments["runId"]))
	revision, ok := runControlPositiveInt64(input.Call.Arguments["expectedRevision"])
	if runID == "" || !ok {
		return nil, errors.New("Run control requires a current Run and revision")
	}
	target, err := d.store.GetAgentRun(ctx, input.Call.Scope, runID)
	if err != nil {
		return nil, err
	}
	if err := validateConversationTaskControl(ctx, d.store, input.Run, target); err != nil {
		return nil, err
	}
	result, err := d.commands.CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: input.Call.Scope, RunID: runID, ExpectedRevision: revision,
		Kind:       AgentRunCommandKind(input.Bound.Action.Name),
		Actor:      models.ActivityActor{Type: "agent", ID: input.Run.Owner.ID},
		Summary:    strings.TrimSpace(fmt.Sprint(input.Call.Arguments["reason"])),
		Visibility: models.ActivityVisibilityScope,
	})
	if err != nil {
		return nil, err
	}
	output := map[string]interface{}{"resourceType": runResourceType, "operation": input.Bound.Action.Name, "run": result.Run}
	if result.Event != nil {
		output["event"] = result.Event
	}
	return output, nil
}

func isRunManagementAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil && bound.Definition.ID == models.RunManagementSkillID &&
		(bound.Action.Name == models.RunActionPause || bound.Action.Name == models.RunActionResume || bound.Action.Name == models.RunActionCancel)
}

func runControlPositiveInt64(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, typed > 0
	case int:
		return int64(typed), typed > 0
	case float64:
		return int64(typed), typed > 0 && typed == float64(int64(typed))
	default:
		return 0, false
	}
}

func ApplicableAgentRunCommands(run *models.AgentRun) []string {
	if run == nil || models.IsTerminalAgentRunStatus(run.Status) || run.Kind == models.RunKindConversation {
		return nil
	}
	if run.Status == models.AgentRunStatusPaused {
		return []string{models.RunActionResume, models.RunActionCancel}
	}
	return []string{models.RunActionPause, models.RunActionCancel}
}
