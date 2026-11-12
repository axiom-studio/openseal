package agentbuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	agentBehaviorCandidateVersion = ".action."
)

type agentBehaviorActionArguments struct {
	ExpectedDeploymentRevision int64                              `json:"expectedDeploymentRevision"`
	DisplayName                *string                            `json:"displayName,omitempty"`
	Purpose                    *string                            `json:"purpose,omitempty"`
	SystemPrompt               *string                            `json:"systemPrompt,omitempty"`
	Personality                *string                            `json:"personality,omitempty"`
	OperatingPrinciples        *[]string                          `json:"operatingPrinciples,omitempty"`
	ApprovalTimeout            *kernelagent.ApprovalTimeoutPolicy `json:"approvalTimeout,omitempty"`
	RemoveApprovalTimeout      bool                               `json:"removeApprovalTimeout,omitempty"`
	Rationale                  string                             `json:"rationale"`
}

// AgentBehaviorActionValidator derives the current Agent from the durable Run
// and emits an exact, secret-free behavior diff before policy or approval
// persistence. It cannot target another Agent.
type AgentBehaviorActionValidator struct {
	agents    *kernelagent.Registry
	endpoints *externalconversation.EndpointService
	teams     models.TeamRosterReader
}

// SetTeamRoster gives the validator the Team catalog that proves a Team
// member is still on its chat's roster.
func (v *AgentBehaviorActionValidator) SetTeamRoster(teams models.TeamRosterReader) {
	if v != nil {
		v.teams = teams
	}
}

func NewAgentBehaviorActionValidator(agents *kernelagent.Registry, endpoints ...*externalconversation.EndpointService) (*AgentBehaviorActionValidator, error) {
	if agents == nil {
		return nil, errors.New("agent registry is required")
	}
	result := &AgentBehaviorActionValidator{agents: agents}
	if len(endpoints) > 0 {
		result.endpoints = endpoints[0]
	}
	return result, nil
}

func (v *AgentBehaviorActionValidator) ResolveActionProposalArguments(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isAgentMutationAction(input.Bound) {
		return nil, false, nil
	}
	arguments := maps.Clone(input.Arguments)
	if v == nil || v.agents == nil || input.Run == nil {
		return nil, true, errors.New("agent behavior action validator is not configured")
	}
	deploymentID, err := agentBehaviorDeploymentID(input.Run)
	if err != nil {
		return nil, true, err
	}
	if err := agentBehaviorRunOnRoster(ctx, v.teams, input.Run); err != nil {
		return nil, true, err
	}
	deployment, err := v.agents.GetDeployment(ctx, capability.ScopeReference{Kind: input.Run.Scope.Kind, ID: input.Run.Scope.ID}, deploymentID)
	if err != nil {
		return nil, true, err
	}
	if _, supplied := arguments["expectedDeploymentRevision"]; !supplied {
		arguments["expectedDeploymentRevision"] = deployment.Revision
	}
	if isAgentConfigureChannelAction(input.Bound) {
		endpointID, _ := arguments["endpointId"].(string)
		if strings.TrimSpace(endpointID) == "" || v.endpoints == nil {
			return nil, true, errors.New("agent channel configuration requires an existing endpoint")
		}
		endpoint, endpointErr := v.endpoints.Get(ctx, input.Run.Scope, endpointID)
		if endpointErr != nil {
			return nil, true, endpointErr
		}
		if endpoint.DeploymentID != deploymentID || endpoint.Owner.Type != models.OwnerTypeAgent || endpoint.Owner.ID != deploymentID {
			return nil, true, errors.New("agent channel endpoint is not owned by the current Agent")
		}
		if _, supplied := arguments["expectedEndpointRevision"]; !supplied {
			arguments["expectedEndpointRevision"] = endpoint.Revision
		}
	}
	return arguments, true, nil
}

func (v *AgentBehaviorActionValidator) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isAgentMutationAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.agents == nil || input.Run == nil || input.Bound.Binding == nil {
		return nil, errors.New("agent behavior action validator is not configured")
	}
	deploymentID, err := agentBehaviorDeploymentID(input.Run)
	if err != nil {
		return nil, err
	}
	if err := agentBehaviorRunOnRoster(ctx, v.teams, input.Run); err != nil {
		return nil, err
	}
	if input.Bound.Binding.DeploymentID != deploymentID {
		return nil, errors.New("agent behavior action is not bound to the current Run's Agent deployment")
	}
	if isAgentConfigureChannelAction(input.Bound) {
		args, deployment, definition, endpoint, candidate, changes, channelErr := resolveAgentChannelAction(ctx, v.agents, v.endpoints, input.Run, input.Arguments)
		if channelErr != nil {
			return nil, channelErr
		}
		return map[string]interface{}{
			"resourceType": models.AgentBehaviorResourceType, "operation": models.AgentActionConfigureChannel,
			"deploymentId": deployment.ID, "definitionId": definition.ID, "baseVersion": definition.Version,
			"endpointId": endpoint.ID, "expectedEndpointRevision": endpoint.Revision,
			"current": agentChannelCurrentValues(definition, endpoint), "changes": changes,
			"candidate": candidate.Channels, "rationale": strings.TrimSpace(args.Rationale),
		}, nil
	}
	args, deployment, definition, changes, err := resolveAgentBehaviorAction(ctx, v.agents, input.Run, input.Arguments)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"resourceType": models.AgentBehaviorResourceType, "operation": models.AgentActionAmendBehavior,
		"deploymentId": deployment.ID, "definitionId": definition.ID, "baseVersion": definition.Version,
		"expectedDeploymentRevision": deployment.Revision,
		"current":                    agentBehaviorCurrentValues(definition, changes),
		"changes":                    changes, "rationale": strings.TrimSpace(args.Rationale),
	}, nil
}

// AgentBehaviorActionDispatcher materializes an approved proposal through the
// canonical immutable Agent amendment and activation lifecycle.
type AgentBehaviorActionDispatcher struct {
	store     models.KernelStore
	agents    *kernelagent.Registry
	endpoints *externalconversation.EndpointService
	fallback  agentrun.ActionDispatcher
}

func NewAgentBehaviorActionDispatcher(store models.KernelStore, agents *kernelagent.Registry, fallback agentrun.ActionDispatcher, endpoints ...*externalconversation.EndpointService) (*AgentBehaviorActionDispatcher, error) {
	if store == nil || agents == nil {
		return nil, errors.New("kernel store and agent registry are required")
	}
	result := &AgentBehaviorActionDispatcher{store: store, agents: agents, fallback: fallback}
	if len(endpoints) > 0 {
		result.endpoints = endpoints[0]
	}
	return result, nil
}

func (d *AgentBehaviorActionDispatcher) DispatchAction(ctx context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	if !models.IsAgentManagementAction(input.Bound) {
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
			err = models.ErrRunNotFound
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
	// The roster can change while a proposal waits for approval.
	if err := agentBehaviorRunOnRoster(ctx, models.TeamRosterReaderOf(d.store), run); err != nil {
		return nil, err
	}
	if isAgentListChannelsAction(input.Bound) {
		return d.listChannels(ctx, input.Call.Scope, deploymentID)
	}
	if isAgentConfigureChannelAction(input.Bound) {
		return d.configureChannel(ctx, input, run)
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
	candidate := cloneAgentDefinitionForAction(definition)
	candidate.Version = candidateVersion
	applyAgentBehaviorArguments(candidate, args)
	amendment, deployment, activation, err := activateAgentAmendment(
		ctx, d.store, d.agents, input.Call.Scope, deployment, definition, candidate, changes, input.Call,
		deploymentID, strings.TrimSpace(args.Rationale), "Approved Agent behavior change from conversation",
	)
	if err != nil {
		return nil, err
	}
	return agentBehaviorActionResult(amendment, deployment, activation, false), nil
}

// activateAgentAmendment proposes candidate as an amendment of deployment
// (idempotent on call.ID), resolves it from the call's approved checkpoint
// when policy requires one, and activates it. candidate.Version must be the
// call's candidate version. Without an approval, proposerID is the actor.
func activateAgentAmendment(
	ctx context.Context,
	store models.ActionStore,
	agents *kernelagent.Registry,
	scope models.Scope,
	deployment *kernelagent.AgentDeployment,
	definition, candidate *kernelagent.AgentDefinition,
	changes map[string]interface{},
	call *models.ActionCall,
	proposerID, rationale, reason string,
) (*kernelagent.DefinitionAmendment, *kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	scopeReference := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	amendment, err := findAgentBehaviorAmendment(ctx, agents, scope, deployment.ID, candidate.Version)
	if err != nil {
		return nil, nil, nil, err
	}
	if amendment == nil {
		additionalAllowedFields := make([]string, 0, len(changes))
		for field := range changes {
			definitionField := agentDefinitionAmendmentField(field)
			if !hasExactAgentActionString(definition.Amendments.AllowedFields, definitionField) &&
				!hasExactAgentActionString(additionalAllowedFields, definitionField) {
				additionalAllowedFields = append(additionalAllowedFields, definitionField)
			}
		}
		if len(additionalAllowedFields) > 0 {
			if call.ApprovalID == "" {
				return nil, nil, nil, errors.New("Agent behavior fields outside its autonomous amendment policy require an approved action checkpoint")
			}
			if _, approvalErr := approvedAgentBehaviorCheckpoint(ctx, store, call); approvalErr != nil {
				return nil, nil, nil, approvalErr
			}
		}
		amendment, err = agents.ProposeAmendment(ctx, kernelagent.ProposeAmendmentRequest{
			Scope: scopeReference, DeploymentID: deployment.ID,
			Candidate: candidate, ProposerType: "agent", ProposerID: proposerID,
			Rationale: rationale, EvidenceRefs: append([]string(nil), call.EvidenceRefs...),
			IdempotencyKey: call.ID, ExpectedDeploymentRevision: deployment.Revision,
			AdditionalAllowedFields: additionalAllowedFields,
		})
		if err != nil {
			return nil, nil, nil, err
		}
	}
	actorType, actorID := "agent", proposerID
	if call.ApprovalID != "" {
		approval, approvalErr := approvedAgentBehaviorCheckpoint(ctx, store, call)
		if approvalErr != nil {
			return nil, nil, nil, approvalErr
		}
		actorType, actorID = approval.DecisionBy.Type, approval.DecisionBy.ID
	}
	if amendment.Status == kernelagent.AmendmentAwaitingApproval {
		if call.ApprovalID == "" {
			return nil, nil, nil, errors.New("agent definition policy requires an approved action checkpoint")
		}
		approval, approvalErr := approvedAgentBehaviorCheckpoint(ctx, store, call)
		if approvalErr != nil {
			return nil, nil, nil, approvalErr
		}
		amendment, err = agents.ResolveAmendmentFromGovernedApproval(ctx, kernelagent.ResolveAmendmentRequest{
			Scope: scopeReference, AmendmentID: amendment.ID,
			ExpectedRevision: amendment.Revision, Approved: true,
			ActorType: approval.DecisionBy.Type, ActorID: approval.DecisionBy.ID, Reason: approval.DecisionReason,
		})
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if amendment.Status != kernelagent.AmendmentReady && amendment.Status != kernelagent.AmendmentApproved {
		return nil, nil, nil, fmt.Errorf("agent amendment is not ready after governed action approval: %s", amendment.Status)
	}
	return agents.ActivateAmendment(ctx, scopeReference, amendment.ID, amendment.Revision, actorType, actorID, reason)
}

func resolveAgentBehaviorAction(
	ctx context.Context,
	agents *kernelagent.Registry,
	run *models.AgentRun,
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
		return args, nil, nil, nil, skillbinding.ErrAgentBehaviorFixed
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

func equalJSON(left, right interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func hasExactAgentActionString(values []string, expected string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == expected {
			return true
		}
	}
	return false
}

// agentBehaviorDeploymentID is the Agent whose behavior and channels the Run
// may change: its acting Agent, in that Agent's own chat or as a Team member
// in a Team chat, exactly as in that member's own chat.
func agentBehaviorDeploymentID(run *models.AgentRun) (string, error) {
	if run == nil {
		return "", errors.New("agent behavior actions require an Agent's own conversation Run or a Team member's Run")
	}
	deploymentID := strings.TrimSpace(run.AssignedAgentID)
	if deploymentID == "" {
		return "", errors.New("agent behavior actions require the Run's acting Agent")
	}
	switch {
	case run.Owner.Type == models.OwnerTypeAgent:
		if run.Owner.ID != deploymentID {
			return "", errors.New("agent behavior action cannot target a Run assigned to another Agent")
		}
	case run.Owner.Type == models.OwnerTypeTeam:
		// agentBehaviorRunOnRoster proves the member is on the Team's roster.
	default:
		return "", errors.New("agent behavior actions require an Agent's own conversation Run or a Team member's Run")
	}
	return deploymentID, nil
}

// agentBehaviorRunOnRoster requires a Team member Run's Agent to be on the
// owning Team's current active roster. An Agent's own Run is its roster of one.
func agentBehaviorRunOnRoster(ctx context.Context, teams models.TeamRosterReader, run *models.AgentRun) error {
	if run == nil || run.Owner.Type != models.OwnerTypeTeam {
		return nil
	}
	if teams == nil {
		return errors.New("agent behavior action requires the current Team roster")
	}
	onRoster, err := models.AgentOnChatRoster(ctx, teams, run.Scope, run.Owner, strings.TrimSpace(run.AssignedAgentID))
	if err != nil {
		return err
	}
	if !onRoster {
		return errors.New("agent behavior action Agent is not on the active Team roster")
	}
	return nil
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
		candidate.OperatingPrinciples = models.TrimmedUniqueStrings(*args.OperatingPrinciples)
	}
	if args.ApprovalTimeout != nil {
		value := *args.ApprovalTimeout
		candidate.Authority.ApprovalTimeout = &value
	}
	if args.RemoveApprovalTimeout {
		candidate.Authority.ApprovalTimeout = nil
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
	if !equalApprovalTimeoutPolicy(base.Authority.ApprovalTimeout, candidate.Authority.ApprovalTimeout) {
		changes["authority.approvalTimeout"] = candidate.Authority.ApprovalTimeout
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
		case "authority.approvalTimeout":
			current[field] = definition.Authority.ApprovalTimeout
		}
	}
	return current
}

func equalApprovalTimeoutPolicy(left, right *kernelagent.ApprovalTimeoutPolicy) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.AfterSeconds == right.AfterSeconds && left.Decision == right.Decision
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
	scope models.Scope,
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

func approvedAgentBehaviorCheckpoint(ctx context.Context, store models.ActionStore, call *models.ActionCall) (*models.ApprovalCheckpoint, error) {
	if strings.TrimSpace(call.ApprovalID) == "" {
		return nil, errors.New("agent definition policy requires an approved action checkpoint")
	}
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil {
		return nil, err
	}
	if approval.Status != models.ApprovalStatusApproved || approval.DecisionBy == nil {
		return nil, errors.New("agent definition policy requires an approved action checkpoint with a decision principal")
	}
	return approval, nil
}

func (d *AgentBehaviorActionDispatcher) replayedResult(ctx context.Context, call *models.ActionCall, run *models.AgentRun) (map[string]interface{}, error) {
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
		"resourceType": models.AgentBehaviorResourceType, "operation": models.AgentActionAmendBehavior, "replayed": replayed,
		"amendment": amendment, "deployment": deployment, "activation": activation,
	}
}

// isAgentBuilderAction matches the builder actions. They run only on the
// current contract; an older binding gets them after its upgrade.
func isAgentBuilderAction(bound *skill.BoundAction) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != models.AgentManagementSkillID ||
		bound.Definition.Version != models.AgentManagementSkillVersion {
		return false
	}
	switch bound.Action.Name {
	case models.AgentActionListAgents, models.AgentActionCreateAgent, models.AgentActionUpdateAgent, models.AgentActionTryAgent:
		return true
	}
	return false
}

func isAgentMutationAction(bound *skill.BoundAction) bool {
	return isAgentBehaviorAction(bound) || isAgentConfigureChannelAction(bound)
}

func isAgentListChannelsAction(bound *skill.BoundAction) bool {
	return models.IsAgentManagementAction(bound) && bound.Action.Name == models.AgentActionListChannels
}

func isAgentConfigureChannelAction(bound *skill.BoundAction) bool {
	return models.IsAgentManagementAction(bound) && bound.Action.Name == models.AgentActionConfigureChannel
}

func isAgentBehaviorAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil &&
		bound.Definition.ID == models.AgentManagementSkillID &&
		models.IsSupportedAgentManagementSkillVersion(bound.Definition.Version) &&
		bound.Action.Name == models.AgentActionAmendBehavior
}
