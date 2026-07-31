package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	AgentManagementSkillID        = "openseal.agents"
	AgentManagementSkillVersion   = "1.1.0"
	AgentActionAmendBehavior      = "amend_behavior"
	AgentManagementEndpoint       = "kernel://agents"
	agentBehaviorResourceType     = "agent_definition"
	agentBehaviorCandidateVersion = ".action."
)

// AgentManagementSkill exposes bounded, self-only behavior amendments to an
// Agent conversation. The target deployment, active definition, and current
// revision are kernel-owned facts and are never model-selected inputs.
func AgentManagementSkill() *skill.Definition {
	return &skill.Definition{
		ID: AgentManagementSkillID, Version: AgentManagementSkillVersion,
		Name: "Agents", Description: "Propose governed changes to the current Agent's behavior.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: AgentManagementEndpoint},
		Actions: map[string]skill.Action{
			AgentActionAmendBehavior: {
				Name:        AgentActionAmendBehavior,
				Description: "Propose changing the current Agent's purpose, system prompt, personality, or operating principles. The kernel enforces the active definition's amendment policy and immutable activation lifecycle.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"expectedDeploymentRevision": map[string]interface{}{
							"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true,
						},
						"displayName":         map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 200},
						"purpose":             map[string]interface{}{"type": "string", "minLength": 1},
						"systemPrompt":        map[string]interface{}{"type": "string", "minLength": 1},
						"personality":         map[string]interface{}{"type": "string"},
						"operatingPrinciples": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true},
						"rationale":           map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"expectedDeploymentRevision", "rationale"},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": agentBehaviorResourceType},
						"operation":    map[string]interface{}{"type": "string", "const": AgentActionAmendBehavior},
						"replayed":     map[string]interface{}{"type": "boolean"},
						"amendment":    map[string]interface{}{"type": "object"},
						"deployment":   map[string]interface{}{"type": "object"},
						"activation":   map[string]interface{}{"type": "object"},
					},
					"required": []interface{}{"resourceType", "operation", "replayed", "amendment", "deployment", "activation"},
				},
			},
		},
	}
}

type agentBehaviorActionArguments struct {
	ExpectedDeploymentRevision int64     `json:"expectedDeploymentRevision"`
	DisplayName                *string   `json:"displayName,omitempty"`
	Purpose                    *string   `json:"purpose,omitempty"`
	SystemPrompt               *string   `json:"systemPrompt,omitempty"`
	Personality                *string   `json:"personality,omitempty"`
	OperatingPrinciples        *[]string `json:"operatingPrinciples,omitempty"`
	Rationale                  string    `json:"rationale"`
}

// AgentBehaviorActionValidator derives the current Agent from the durable Run
// and emits an exact, secret-free behavior diff before policy or approval
// persistence. It cannot target another Agent.
type AgentBehaviorActionValidator struct{ agents *kernelagent.Registry }

func NewAgentBehaviorActionValidator(agents *kernelagent.Registry) (*AgentBehaviorActionValidator, error) {
	if agents == nil {
		return nil, errors.New("agent registry is required")
	}
	return &AgentBehaviorActionValidator{agents: agents}, nil
}

func (v *AgentBehaviorActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isAgentBehaviorAction(input.Bound) {
		return nil, false, nil
	}
	arguments := cloneMap(input.Arguments)
	if _, supplied := arguments["expectedDeploymentRevision"]; supplied {
		return arguments, true, nil
	}
	if v == nil || v.agents == nil || input.Run == nil {
		return nil, true, errors.New("agent behavior action validator is not configured")
	}
	deploymentID, err := agentBehaviorDeploymentID(input.Run)
	if err != nil {
		return nil, true, err
	}
	deployment, err := v.agents.GetDeployment(ctx, capability.ScopeReference{Kind: input.Run.Scope.Kind, ID: input.Run.Scope.ID}, deploymentID)
	if err != nil {
		return nil, true, err
	}
	arguments["expectedDeploymentRevision"] = deployment.Revision
	return arguments, true, nil
}

func (v *AgentBehaviorActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isAgentBehaviorAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.agents == nil || input.Run == nil || input.Bound.Binding == nil {
		return nil, errors.New("agent behavior action validator is not configured")
	}
	deploymentID, err := agentBehaviorDeploymentID(input.Run)
	if err != nil {
		return nil, err
	}
	if input.Bound.Binding.DeploymentID != deploymentID {
		return nil, errors.New("agent behavior action is not bound to the current Run's Agent deployment")
	}
	args, deployment, definition, changes, err := resolveAgentBehaviorAction(ctx, v.agents, input.Run, input.Arguments)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"resourceType": agentBehaviorResourceType, "operation": AgentActionAmendBehavior,
		"deploymentId": deployment.ID, "definitionId": definition.ID, "baseVersion": definition.Version,
		"expectedDeploymentRevision": deployment.Revision,
		"current":                    agentBehaviorCurrentValues(definition, changes),
		"changes":                    changes, "rationale": strings.TrimSpace(args.Rationale),
	}, nil
}

// AgentBehaviorActionDispatcher materializes an approved proposal through the
// canonical immutable Agent amendment and activation lifecycle.
type AgentBehaviorActionDispatcher struct {
	store    KernelStore
	agents   *kernelagent.Registry
	fallback ActionDispatcher
}

func NewAgentBehaviorActionDispatcher(store KernelStore, agents *kernelagent.Registry, fallback ActionDispatcher) (*AgentBehaviorActionDispatcher, error) {
	if store == nil || agents == nil {
		return nil, errors.New("kernel store and agent registry are required")
	}
	return &AgentBehaviorActionDispatcher{store: store, agents: agents, fallback: fallback}, nil
}

func (d *AgentBehaviorActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isAgentBehaviorAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || d.agents == nil || input.Call == nil || input.Bound.Binding == nil {
		return nil, errors.New("agent behavior action dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	deploymentID, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return nil, err
	}
	if input.Call.DeploymentID != deploymentID || input.Bound.Binding.DeploymentID != deploymentID {
		return nil, errors.New("agent behavior action cannot target another Agent deployment")
	}
	var args agentBehaviorActionArguments
	if err := decodeAgentBehaviorArguments(input.Arguments, &args); err != nil {
		return nil, err
	}
	_, deployment, definition, changes, err := resolveAgentBehaviorAction(ctx, d.agents, run, input.Arguments)
	if errors.Is(err, kernelagent.ErrRevisionConflict) {
		return d.replayedResult(ctx, input.Call, run)
	}
	if err != nil {
		return nil, err
	}
	candidateVersion := agentBehaviorActionCandidateVersion(definition.Version, input.Call.ID)
	if deployment.ActiveVersion == candidateVersion {
		return d.replayedResult(ctx, input.Call, run)
	}
	amendment, err := findAgentBehaviorAmendment(ctx, d.agents, input.Call.Scope, deployment.ID, candidateVersion)
	if err != nil {
		return nil, err
	}
	if amendment == nil {
		additionalAllowedFields := make([]string, 0, len(changes))
		for field := range changes {
			if !containsString(definition.Amendments.AllowedFields, field) {
				additionalAllowedFields = append(additionalAllowedFields, field)
			}
		}
		if len(additionalAllowedFields) > 0 {
			if input.Call.ApprovalID == "" {
				return nil, errors.New("Agent behavior fields outside its autonomous amendment policy require an approved action checkpoint")
			}
			if _, approvalErr := approvedAgentBehaviorCheckpoint(ctx, d.store, input.Call); approvalErr != nil {
				return nil, approvalErr
			}
		}
		candidate := cloneAgentDefinitionForAction(definition)
		candidate.Version = candidateVersion
		applyAgentBehaviorArguments(candidate, args)
		amendment, err = d.agents.ProposeAmendment(ctx, kernelagent.ProposeAmendmentRequest{
			Scope: capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, DeploymentID: deployment.ID,
			Candidate: candidate, ProposerType: "agent", ProposerID: deploymentID,
			Rationale: strings.TrimSpace(args.Rationale), EvidenceRefs: append([]string(nil), input.Call.EvidenceRefs...),
			IdempotencyKey: input.Call.ID, ExpectedDeploymentRevision: deployment.Revision,
			AdditionalAllowedFields: additionalAllowedFields,
		})
		if err != nil {
			return nil, err
		}
	}
	actorType, actorID := "agent", deploymentID
	if input.Call.ApprovalID != "" {
		approval, approvalErr := approvedAgentBehaviorCheckpoint(ctx, d.store, input.Call)
		if approvalErr != nil {
			return nil, approvalErr
		}
		actorType, actorID = approval.DecisionBy.Type, approval.DecisionBy.ID
	}
	if amendment.Status == kernelagent.AmendmentAwaitingApproval {
		if input.Call.ApprovalID == "" {
			return nil, errors.New("agent definition policy requires an approved action checkpoint")
		}
		approval, approvalErr := approvedAgentBehaviorCheckpoint(ctx, d.store, input.Call)
		if approvalErr != nil {
			return nil, approvalErr
		}
		amendment, err = d.agents.ResolveAmendmentFromGovernedApproval(ctx, kernelagent.ResolveAmendmentRequest{
			Scope: capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, AmendmentID: amendment.ID,
			ExpectedRevision: amendment.Revision, Approved: true,
			ActorType: approval.DecisionBy.Type, ActorID: approval.DecisionBy.ID, Reason: approval.DecisionReason,
		})
		if err != nil {
			return nil, err
		}
	}
	if amendment.Status != kernelagent.AmendmentReady && amendment.Status != kernelagent.AmendmentApproved {
		return nil, fmt.Errorf("agent amendment is not ready after governed action approval: %s", amendment.Status)
	}
	amendment, deployment, activation, err := d.agents.ActivateAmendment(
		ctx, capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, amendment.ID, amendment.Revision,
		actorType, actorID, "Approved Agent behavior change from conversation",
	)
	if err != nil {
		return nil, err
	}
	return agentBehaviorActionResult(amendment, deployment, activation, false), nil
}

func resolveAgentBehaviorAction(
	ctx context.Context,
	agents *kernelagent.Registry,
	run *AgentRun,
	arguments map[string]interface{},
) (agentBehaviorActionArguments, *kernelagent.AgentDeployment, *kernelagent.AgentDefinition, map[string]interface{}, error) {
	var args agentBehaviorActionArguments
	if err := decodeAgentBehaviorArguments(arguments, &args); err != nil {
		return args, nil, nil, nil, err
	}
	if args.ExpectedDeploymentRevision < 1 || strings.TrimSpace(args.Rationale) == "" {
		return args, nil, nil, nil, errors.New("expectedDeploymentRevision and rationale are required")
	}
	deploymentID, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return args, nil, nil, nil, err
	}
	scope := capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	deployment, err := agents.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return args, nil, nil, nil, err
	}
	if deployment.Revision != args.ExpectedDeploymentRevision {
		return args, nil, nil, nil, kernelagent.ErrRevisionConflict
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return args, nil, nil, nil, err
	}
	if !definition.Amendments.AgentMayPropose {
		return args, nil, nil, nil, errors.New("agent definition policy does not allow Agent-proposed amendments")
	}
	candidate := cloneAgentDefinitionForAction(definition)
	applyAgentBehaviorArguments(candidate, args)
	if err := candidate.Validate(); err != nil {
		return args, nil, nil, nil, err
	}
	changes := agentBehaviorChanges(definition, candidate)
	if len(changes) == 0 {
		return args, nil, nil, nil, errors.New("agent behavior amendment requires at least one changed field")
	}
	return args, deployment, definition, changes, nil
}

func agentBehaviorDeploymentID(run *AgentRun) (string, error) {
	if run == nil || run.Owner.Type != OwnerTypeAgent || strings.TrimSpace(run.Owner.ID) == "" {
		return "", errors.New("agent behavior actions require an Agent-owned conversation Run")
	}
	deploymentID := strings.TrimSpace(run.Owner.ID)
	if assigned := strings.TrimSpace(run.AssignedAgentID); assigned != "" && assigned != deploymentID {
		return "", errors.New("agent behavior action cannot target a Run assigned to another Agent")
	}
	return deploymentID, nil
}

func decodeAgentBehaviorArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid agent behavior action: %w", err)
	}
	return nil
}

func applyAgentBehaviorArguments(candidate *kernelagent.AgentDefinition, args agentBehaviorActionArguments) {
	if args.DisplayName != nil {
		candidate.DisplayName = strings.TrimSpace(*args.DisplayName)
	}
	if args.Purpose != nil {
		candidate.Purpose = strings.TrimSpace(*args.Purpose)
	}
	if args.SystemPrompt != nil {
		candidate.SystemPrompt = strings.TrimSpace(*args.SystemPrompt)
	}
	if args.Personality != nil {
		candidate.Personality = strings.TrimSpace(*args.Personality)
	}
	if args.OperatingPrinciples != nil {
		candidate.OperatingPrinciples = normalizedActionStrings(*args.OperatingPrinciples)
	}
}

func agentBehaviorChanges(base, candidate *kernelagent.AgentDefinition) map[string]interface{} {
	changes := make(map[string]interface{})
	if base.DisplayName != candidate.DisplayName {
		changes["displayName"] = candidate.DisplayName
	}
	if base.Purpose != candidate.Purpose {
		changes["purpose"] = candidate.Purpose
	}
	if base.SystemPrompt != candidate.SystemPrompt {
		changes["systemPrompt"] = candidate.SystemPrompt
	}
	if base.Personality != candidate.Personality {
		changes["personality"] = candidate.Personality
	}
	if !equalStrings(base.OperatingPrinciples, candidate.OperatingPrinciples) {
		changes["operatingPrinciples"] = append([]string(nil), candidate.OperatingPrinciples...)
	}
	return changes
}

func agentBehaviorCurrentValues(definition *kernelagent.AgentDefinition, changes map[string]interface{}) map[string]interface{} {
	current := make(map[string]interface{}, len(changes))
	for field := range changes {
		switch field {
		case "displayName":
			current[field] = definition.DisplayName
		case "purpose":
			current[field] = definition.Purpose
		case "systemPrompt":
			current[field] = definition.SystemPrompt
		case "personality":
			current[field] = definition.Personality
		case "operatingPrinciples":
			current[field] = append([]string(nil), definition.OperatingPrinciples...)
		}
	}
	return current
}

func normalizedActionStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneAgentDefinitionForAction(value *kernelagent.AgentDefinition) *kernelagent.AgentDefinition {
	if value == nil {
		return nil
	}
	var result kernelagent.AgentDefinition
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	result.Digest = ""
	return &result
}

func agentBehaviorActionCandidateVersion(baseVersion, actionID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(actionID)))
	suffix := agentBehaviorCandidateVersion + hex.EncodeToString(digest[:6])
	baseVersion = strings.TrimSpace(baseVersion)
	if len(baseVersion)+len(suffix) > 128 {
		baseVersion = baseVersion[:128-len(suffix)]
	}
	return baseVersion + suffix
}

func findAgentBehaviorAmendment(
	ctx context.Context,
	agents *kernelagent.Registry,
	scope Scope,
	deploymentID, version string,
) (*kernelagent.DefinitionAmendment, error) {
	amendments, err := agents.ListAmendments(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if err != nil {
		return nil, err
	}
	for _, amendment := range amendments {
		if amendment != nil && amendment.Candidate.Version == version {
			return amendment, nil
		}
	}
	return nil, nil
}

func approvedAgentBehaviorCheckpoint(ctx context.Context, store ActionStore, call *ActionCall) (*ApprovalCheckpoint, error) {
	if strings.TrimSpace(call.ApprovalID) == "" {
		return nil, errors.New("agent definition policy requires an approved action checkpoint")
	}
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil {
		return nil, err
	}
	if approval.Status != ApprovalStatusApproved || approval.DecisionBy == nil {
		return nil, errors.New("agent definition policy requires an approved action checkpoint with a decision principal")
	}
	return approval, nil
}

func (d *AgentBehaviorActionDispatcher) replayedResult(ctx context.Context, call *ActionCall, run *AgentRun) (map[string]interface{}, error) {
	deploymentID, err := agentBehaviorDeploymentID(run)
	if err != nil {
		return nil, err
	}
	scope := capability.ScopeReference{Kind: call.Scope.Kind, ID: call.Scope.ID}
	deployment, err := d.agents.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	expectedVersion := agentBehaviorActionCandidateVersion("", call.ID)
	if !strings.HasSuffix(deployment.ActiveVersion, expectedVersion) {
		return nil, kernelagent.ErrRevisionConflict
	}
	amendment, err := findAgentBehaviorAmendment(ctx, d.agents, call.Scope, deploymentID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if amendment == nil || amendment.Status != kernelagent.AmendmentActivated {
		return nil, kernelagent.ErrRevisionConflict
	}
	activations, err := d.agents.ListActivations(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	for index := range activations {
		if activations[index].ToVersion == deployment.ActiveVersion {
			activation := activations[index]
			return agentBehaviorActionResult(amendment, deployment, &activation, true), nil
		}
	}
	return nil, kernelagent.ErrRevisionConflict
}

func agentBehaviorActionResult(
	amendment *kernelagent.DefinitionAmendment,
	deployment *kernelagent.AgentDeployment,
	activation *kernelagent.DefinitionActivation,
	replayed bool,
) map[string]interface{} {
	return map[string]interface{}{
		"resourceType": agentBehaviorResourceType, "operation": AgentActionAmendBehavior, "replayed": replayed,
		"amendment": amendment, "deployment": deployment, "activation": activation,
	}
}

func isAgentBehaviorAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil &&
		bound.Definition.ID == AgentManagementSkillID &&
		bound.Definition.Version == AgentManagementSkillVersion &&
		bound.Action.Name == AgentActionAmendBehavior
}
