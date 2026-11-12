package agentbuilder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// AgentProvisioner is the Engine side of the builder actions: creating
// deployments and placing them in spaces. *openseal.Engine implements it.
type AgentProvisioner interface {
	// CreateAgentDeploymentInSpace creates the deployment as a member of the
	// space and creates its control conversation there.
	CreateAgentDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error)
	// EnsureAgentControlConversation returns the Agent's control
	// conversation, creating it when it is missing. It is idempotent.
	EnsureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment) (*models.Conversation, error)
	// CreateTeamDeploymentInSpace creates the Team as a member of the
	// space; its Agents must already be in it.
	CreateTeamDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelteam.Deployment, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error)
	ListSpaces(ctx context.Context, scope models.Scope) ([]*models.Space, error)
	ListSpaceMembers(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpaceMember, error)
	PutSpaceMember(ctx context.Context, request portfolio.PutSpaceMemberRequest) (*models.SpaceMember, bool, error)
}

const (
	agentBuilderDefinitionVersion  = "1.0.0"
	agentBuilderProvenanceSource   = "agent-builder"
	agentBuilderCreateReason       = "Created in chat after review"
	agentBuilderUpdateReason       = "Changed in chat after review"
	agentBuilderCreateFailedReason = "Creating it in chat failed"
)

// ActionDispatcher executes list_agents, create_agent,
// update_agent, try_agent and openseal.teams create_team. Every write step is idempotent on the
// ActionCall, so a retry or a crash-resumed execution converges on the same
// Agent.
type ActionDispatcher struct {
	*AgentInstaller
	fallback agentrun.ActionDispatcher
}

// NewActionDispatcher wraps fallback, which receives every other
// action. Every dispatcher of an Engine shares its one installer.
func NewActionDispatcher(installer *AgentInstaller, fallback agentrun.ActionDispatcher) (*ActionDispatcher, error) {
	if installer == nil {
		return nil, errors.New("agent builder dispatcher requires the agent installer")
	}
	return &ActionDispatcher{AgentInstaller: installer, fallback: fallback}, nil
}

func (d *ActionDispatcher) DispatchAction(ctx context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	if !isAgentBuilderDispatchedAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.AgentInstaller == nil || input.Call == nil || input.Bound.Binding == nil {
		return nil, errors.New("agent builder dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = models.ErrRunNotFound
		}
		return nil, err
	}
	builderID := strings.TrimSpace(run.AssignedAgentID)
	if builderID == "" || input.Call.DeploymentID != builderID || input.Bound.Binding.DeploymentID != builderID {
		return nil, errors.New("agent builder action is not bound to the current Run's Agent deployment")
	}
	switch input.Bound.Action.Name {
	case models.AgentActionListAgents:
		return d.listAgents(ctx, input.Call.Scope, builderID, input.Arguments)
	case models.AgentActionCreateAgent:
		return d.createAgent(ctx, input, run)
	case models.AgentActionUpdateAgent:
		return d.updateAgent(ctx, input, run)
	case models.AgentActionTryAgent:
		return d.tryAgent(ctx, input, run)
	case models.TeamActionCreateTeam:
		return d.createTeam(ctx, input, run)
	}
	return nil, errors.New("unsupported agent builder action")
}

func isAgentBuilderDispatchedAction(bound *skill.BoundAction) bool {
	if isTeamCreateAction(bound) {
		return true
	}
	if !isAgentBuilderAction(bound) {
		return false
	}
	switch bound.Action.Name {
	case models.AgentActionListAgents, models.AgentActionCreateAgent, models.AgentActionUpdateAgent, models.AgentActionTryAgent:
		return true
	}
	return false
}

// approvedAgentBuilderCheckpoint returns the call's approval. Only a person
// may approve an agent builder card.
func approvedAgentBuilderCheckpoint(ctx context.Context, store models.ActionStore, call *models.ActionCall) (*models.ApprovalCheckpoint, error) {
	if strings.TrimSpace(call.ApprovalID) == "" {
		return nil, errors.New("agent builder changes run only after a person approves the card")
	}
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil {
		return nil, err
	}
	if approval.Status != models.ApprovalStatusApproved || approval.DecisionBy == nil {
		return nil, errors.New("agent builder changes run only after a person approves the card")
	}
	if approval.DecisionBy.Type != "user" || strings.TrimSpace(approval.DecisionBy.ID) == "" {
		return nil, fmt.Errorf("agent builder cards can be approved only by a person, not %s", approval.DecisionBy.Type)
	}
	return approval, nil
}

// agentBuilderResourceID derives a definition or deployment id from the
// call, in the 21-character form of the existing ids.
func agentBuilderResourceID(kind string, scope models.Scope, callID string) string {
	digest := sha256.Sum256([]byte(kind + ":" + scope.Kind + ":" + scope.ID + ":" + callID))
	return "a" + base64.RawURLEncoding.EncodeToString(digest[:])[:20]
}

func agentBuilderBindingID(deploymentID, skillID string) string {
	return "builder:" + deploymentID + ":" + skillID
}

func (d *ActionDispatcher) listAgents(ctx context.Context, scope models.Scope, builderID string, arguments map[string]interface{}) (map[string]interface{}, error) {
	var args models.AgentListArguments
	if err := decodeAgentBuilderArguments(arguments, &args); err != nil {
		return nil, err
	}
	query := models.NormalizedAgentName(args.Query)
	deployments, err := d.agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{
		Scope: models.ScopeReference(scope), ExcludeStatuses: []kernelagent.RolloutStatus{kernelagent.RolloutRetired},
	})
	if err != nil {
		return nil, err
	}
	memberships, err := d.agentSpaceIDs(ctx, scope)
	if err != nil {
		return nil, err
	}
	agents := make([]map[string]interface{}, 0, len(deployments))
	for _, deployment := range deployments {
		if deployment == nil {
			continue
		}
		definition, err := d.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
		if err != nil {
			return nil, err
		}
		name := agentDeploymentDisplayName(deployment, definition)
		if query != "" && !strings.Contains(models.NormalizedAgentName(name), query) &&
			!strings.Contains(models.NormalizedAgentName(definition.Purpose), query) {
			continue
		}
		spaceIDs := memberships[deployment.ID]
		if spaceIDs == nil {
			spaceIDs = []string{}
		}
		agents = append(agents, map[string]interface{}{
			"id": deployment.ID, "displayName": name, "purpose": definition.Purpose,
			"rolloutStatus": deployment.RolloutStatus, "spaceIds": spaceIDs,
			"manageable": deployment.ID != builderID && !skillbinding.AgentDefinitionPolicyFixed(definition),
		})
	}
	return agentBuilderJSONMap(map[string]interface{}{"resourceType": "agent_list", "agents": agents})
}

// agentSpaceIDs maps each Agent deployment to the spaces it is in.
func (d *ActionDispatcher) agentSpaceIDs(ctx context.Context, scope models.Scope) (map[string][]string, error) {
	spaces, err := d.provision.ListSpaces(ctx, scope)
	if err != nil {
		return nil, err
	}
	result := map[string][]string{}
	for _, space := range spaces {
		if space == nil {
			continue
		}
		members, err := d.provision.ListSpaceMembers(ctx, scope, space.ID)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			if member != nil && member.Type == models.SpaceMemberAgent {
				result[member.ID] = append(result[member.ID], space.ID)
			}
		}
	}
	return result, nil
}

func agentDeploymentDisplayName(deployment *kernelagent.AgentDeployment, definition *kernelagent.AgentDefinition) string {
	if name := strings.TrimSpace(deployment.DisplayName); name != "" {
		return name
	}
	if definition == nil {
		return ""
	}
	return definition.DisplayName
}

func (d *ActionDispatcher) createAgent(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun) (map[string]interface{}, error) {
	call := input.Call
	approval, err := approvedAgentBuilderCheckpoint(ctx, d.store, call)
	if err != nil {
		return nil, err
	}
	scope := call.Scope
	var args models.AgentCreateArguments
	if err := decodeAgentBuilderArguments(input.Arguments, &args); err != nil {
		return nil, err
	}
	args.AgentProfileArguments = args.AgentProfileArguments.Normalized()
	args.SpaceID = strings.TrimSpace(args.SpaceID)
	if err := models.ValidateAgentProfileText(args.AgentProfileArguments); err != nil {
		return nil, err
	}
	grants, err := resolveAgentSkillGrants(ctx, d.catalog, args.Skills)
	if err != nil {
		return nil, err
	}
	approver := approval.DecisionBy.ID
	built, err := d.build(ctx, agentBuildInput{
		Scope:        scope,
		DefinitionID: agentBuilderResourceID("agent-definition", scope, call.ID),
		DeploymentID: agentBuilderResourceID("agent-deployment", scope, call.ID),
		Profile:      args.AgentProfileArguments,
		SpaceID:      args.SpaceID,
		Grants:       grants,
		MaximumRisk:  agentGrantsMaximumRisk("", grants),
		Provenance:   workforce.DefinitionProvenance{Source: agentBuilderProvenanceSource, Reference: call.ID, CreatedBy: approver},
		Approver:     approver,
		CallRef:      call.ID,
		RunID:        run.ID,
		Reason:       agentBuilderCreateReason,
	})
	if errors.Is(err, ErrAgentNameTaken) {
		return nil, fmt.Errorf("%w. Propose create_agent again with another name", err)
	}
	var retry agentBuildRetryable
	if errors.As(err, &retry) {
		return nil, fmt.Errorf("%w; propose create_agent again", err)
	}
	if err != nil {
		return nil, err
	}
	definition, deploymentID, space := built.definition, built.deployment.ID, built.space
	connections := make([]map[string]interface{}, 0, len(args.Connections))
	for _, connection := range args.Connections {
		connections = append(connections, map[string]interface{}{
			"skillId": strings.TrimSpace(connection.SkillID), "skillVersion": strings.TrimSpace(connection.SkillVersion),
			"sourceIdentity": strings.TrimSpace(connection.SourceIdentity),
		})
	}
	message := fmt.Sprintf("Created %s; its agent id is %s (use it for agent cards and later updates). Offer 'Chat with %s'.", definition.DisplayName, deploymentID, definition.DisplayName)
	if len(connections) > 0 {
		message += fmt.Sprintf(" For each connection, call openseal.skills.request_connection with targetAgentId=%s.", deploymentID)
	}
	return agentBuilderJSONMap(map[string]interface{}{
		"resourceType": models.AgentProfileResourceType, "operation": models.AgentActionCreateAgent,
		// The reviewed profile is already in the conversation; the full
		// definition and deployment would repeat it and put the definition id
		// first, which the model mistakes for the Agent's id.
		"agentId": deploymentID, "deploymentId": deploymentID, "displayName": definition.DisplayName, "replayed": built.replayed,
		"activation": built.activation,
		"bindings":   built.bindings, "space": map[string]interface{}{"id": space.ID, "name": space.Name},
		"connections": connections, "chat": map[string]interface{}{"conversationId": built.conversation.ID},
		"message": message,
	})
}

func (d *ActionDispatcher) updateAgent(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun) (map[string]interface{}, error) {
	call := input.Call
	approval, err := approvedAgentBuilderCheckpoint(ctx, d.store, call)
	if err != nil {
		return nil, err
	}
	scope := call.Scope
	reference := models.ScopeReference(scope)
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	builderID := strings.TrimSpace(run.AssignedAgentID)
	var args models.AgentUpdateArguments
	if err := decodeAgentBuilderArguments(input.Arguments, &args); err != nil {
		return nil, err
	}
	args.AgentID = strings.TrimSpace(args.AgentID)
	if args.AgentID == "" || args.AgentID == builderID {
		return nil, errors.New("update_agent requires another Agent's agentId")
	}
	deployment, err := d.agents.GetDeployment(ctx, reference, args.AgentID)
	if err != nil {
		return nil, skillbinding.AgentBuilderTargetError(args.AgentID, err)
	}
	// Removals an earlier attempt already made are done; the rest must
	// still be enabled bindings of the target.
	arguments := maps.Clone(input.Arguments)
	pendingRemovals := make([]interface{}, 0, len(args.RemoveSkillBindingIDs))
	for _, bindingID := range args.RemoveSkillBindingIDs {
		bindingID = strings.TrimSpace(bindingID)
		binding, err := d.catalog.GetBinding(ctx, skillScope, args.AgentID, bindingID)
		if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
			return nil, err
		}
		if binding != nil && !binding.Disabled {
			pendingRemovals = append(pendingRemovals, bindingID)
		}
	}
	if len(args.RemoveSkillBindingIDs) > 0 {
		arguments["removeSkillBindingIds"] = pendingRemovals
	}
	profileRequested := args.DisplayName != nil || args.Purpose != nil || args.SystemPrompt != nil ||
		args.Personality != nil || args.OperatingPrinciples != nil
	profileApplied := strings.HasSuffix(deployment.ActiveVersion, agentBehaviorActionCandidateVersion("", call.ID))
	onlyDoneRemovals := !profileRequested && args.Model == nil && len(args.AddSkills) == 0 && len(pendingRemovals) == 0
	replayed := profileApplied || onlyDoneRemovals

	var amendment *kernelagent.DefinitionAmendment
	var activation *kernelagent.DefinitionActivation
	var grants []resolvedAgentSkillGrant
	var removals []string
	model := args.Model
	if replayed {
		if profileApplied {
			if amendment, err = findAgentBehaviorAmendment(ctx, d.agents, scope, deployment.ID, deployment.ActiveVersion); err != nil {
				return nil, err
			}
			if activation, err = d.agentActivationTo(ctx, reference, deployment.ID, deployment.ActiveVersion); err != nil {
				return nil, err
			}
		}
		if grants, err = resolveAgentSkillGrants(ctx, d.catalog, args.AddSkills); err != nil {
			return nil, err
		}
		for _, bindingID := range pendingRemovals {
			removals = append(removals, bindingID.(string))
		}
	} else {
		resolved, err := resolveAgentUpdate(ctx, d.agents, d.teams, d.catalog, d.host, scope, builderID, arguments)
		if errors.Is(err, kernelagent.ErrRevisionConflict) {
			return nil, fmt.Errorf("agent %q changed while the card waited; use list_agents and propose update_agent again", args.AgentID)
		}
		if err != nil {
			return nil, err
		}
		if resolved.authorityRaised {
			// An Agent amendment can't widen its own authority; the person
			// who approved the card activates the new version.
			candidate := cloneAgentDefinitionForAction(resolved.candidate)
			candidate.Version = agentBehaviorActionCandidateVersion(resolved.definition.Version, call.ID)
			deployment, activation, err = d.activateApprovedAgentVersion(ctx, reference, resolved.deployment, candidate, approval.DecisionBy.ID)
			if err != nil {
				return nil, err
			}
		} else if len(resolved.changes) > 0 {
			candidate := cloneAgentDefinitionForAction(resolved.candidate)
			candidate.Version = agentBehaviorActionCandidateVersion(resolved.definition.Version, call.ID)
			amendment, deployment, activation, err = activateAgentAmendment(
				ctx, d.store, d.agents, scope, resolved.deployment, resolved.definition, candidate, resolved.changes, call,
				builderID, strings.TrimSpace(args.Rationale), agentBuilderUpdateReason,
			)
			if err != nil {
				return nil, err
			}
		}
		grants, model = resolved.skills, resolved.model
		for _, removed := range resolved.removed {
			removals = append(removals, removed.binding.ID)
		}
	}
	actor := skill.BindingActor{Type: approval.DecisionBy.Type, ID: approval.DecisionBy.ID}
	bindings := make([]interface{}, 0, len(grants))
	for _, grant := range grants {
		binding, err := d.grantAgentSkill(ctx, scope, deployment.ID, grant.grant, grant.credentials, actor, agentBuilderUpdateReason)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, skillbinding.SecretSafeBindingPreview(binding))
	}
	for _, bindingID := range removals {
		if err := d.disableAgentSkill(ctx, skillScope, deployment.ID, bindingID, actor); err != nil {
			return nil, err
		}
	}
	removedBindings := make([]interface{}, 0, len(args.RemoveSkillBindingIDs))
	for _, bindingID := range args.RemoveSkillBindingIDs {
		binding, err := d.catalog.GetBinding(ctx, skillScope, deployment.ID, strings.TrimSpace(bindingID))
		if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
			return nil, err
		}
		if binding != nil {
			removedBindings = append(removedBindings, skillbinding.SecretSafeBindingPreview(binding))
		}
	}
	if model != nil {
		if err := d.prepareAgentModel(ctx, AgentModelRequest{
			Scope: reference, DeploymentID: deployment.ID, Model: strings.TrimSpace(*model),
			OwnerUserID: approval.DecisionBy.ID, ActionCallID: call.ID, Updated: true,
		}); err != nil {
			return nil, err
		}
	}
	definition, err := d.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	name := agentDeploymentDisplayName(deployment, definition)
	if d.host != nil {
		d.host.AfterAgentCreated(ctx, AgentCreated{
			Scope: scope, DeploymentID: deployment.ID, DisplayName: name, Purpose: definition.Purpose,
			ActionCallID: call.ID, RunID: run.ID, BillingUserID: approval.DecisionBy.ID, Updated: true,
		})
	}
	result := map[string]interface{}{
		"resourceType": models.AgentProfileResourceType, "operation": models.AgentActionUpdateAgent,
		"agentId": deployment.ID, "deploymentId": deployment.ID, "displayName": name, "replayed": replayed,
		"bindings": bindings, "removedBindings": removedBindings,
		"message": fmt.Sprintf("Updated %s; its agent id is %s.", name, deployment.ID),
	}
	if amendment != nil {
		result["amendment"] = amendment
	}
	if activation != nil {
		result["activation"] = activation
	}
	return agentBuilderJSONMap(result)
}
