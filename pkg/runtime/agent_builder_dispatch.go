package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// AgentProvisioner is the Engine side of the builder actions: creating
// deployments and placing them in spaces. *openseal.Engine implements it.
type AgentProvisioner interface {
	// CreateAgentDeployment creates the deployment, places it in Personal
	// when it has no space and creates its control conversation.
	CreateAgentDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error)
	// EnsureAgentControlConversation returns the Agent's control
	// conversation, creating it when it is missing. It is idempotent.
	EnsureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment) (*Conversation, error)
	CreateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error)
	ListSpaces(ctx context.Context, scope Scope) ([]*Space, error)
	ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error)
	PutSpaceMember(ctx context.Context, request PutSpaceMemberRequest) (*SpaceMember, bool, error)
	DeleteSpaceMember(ctx context.Context, scope Scope, spaceID string, memberType SpaceMemberType, memberID string) error
}

const (
	agentBuilderDefinitionVersion = "1.0.0"
	agentBuilderProvenanceSource  = "agent-builder"
	agentBuilderCreateReason      = "Created in chat after review"
	agentBuilderUpdateReason      = "Changed in chat after review"
)

// AgentBuilderActionDispatcher executes list_agents, create_agent,
// update_agent, try_agent and openseal.teams create_team. Every write step is idempotent on the
// ActionCall, so a retry or a crash-resumed execution converges on the same
// Agent.
type AgentBuilderActionDispatcher struct {
	store     KernelStore
	agents    *kernelagent.Registry
	teams     *kernelteam.Registry
	catalog   *skill.Catalog
	provision AgentProvisioner
	host      AgentBuilderHost
	fallback  ActionDispatcher
}

// NewAgentBuilderActionDispatcher wraps fallback, which receives every other
// action. host may be nil: model choices are then refused at proposal time
// and nothing runs after creation.
func NewAgentBuilderActionDispatcher(
	store KernelStore,
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	catalog *skill.Catalog,
	provision AgentProvisioner,
	host AgentBuilderHost,
	fallback ActionDispatcher,
) (*AgentBuilderActionDispatcher, error) {
	if store == nil || agents == nil || teams == nil || catalog == nil || provision == nil {
		return nil, errors.New("agent builder dispatcher requires the kernel store, the Agent and Team registries, the Skill catalog and a provisioner")
	}
	return &AgentBuilderActionDispatcher{
		store: store, agents: agents, teams: teams, catalog: catalog,
		provision: provision, host: host, fallback: fallback,
	}, nil
}

func (d *AgentBuilderActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isAgentBuilderDispatchedAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || input.Call == nil || input.Bound.Binding == nil {
		return nil, errors.New("agent builder dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	builderID := strings.TrimSpace(run.AssignedAgentID)
	if builderID == "" || input.Call.DeploymentID != builderID || input.Bound.Binding.DeploymentID != builderID {
		return nil, errors.New("agent builder action is not bound to the current Run's Agent deployment")
	}
	switch input.Bound.Action.Name {
	case AgentActionListAgents:
		return d.listAgents(ctx, input.Call.Scope, builderID, input.Arguments)
	case AgentActionCreateAgent:
		return d.createAgent(ctx, input, run)
	case AgentActionUpdateAgent:
		return d.updateAgent(ctx, input, run)
	case AgentActionTryAgent:
		return d.tryAgent(ctx, input, run)
	case TeamActionCreateTeam:
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
	case AgentActionListAgents, AgentActionCreateAgent, AgentActionUpdateAgent, AgentActionTryAgent:
		return true
	}
	return false
}

// approvedAgentBuilderCheckpoint returns the call's approval. Only a person
// may approve an agent builder card.
func approvedAgentBuilderCheckpoint(ctx context.Context, store ActionStore, call *ActionCall) (*ApprovalCheckpoint, error) {
	if strings.TrimSpace(call.ApprovalID) == "" {
		return nil, errors.New("agent builder changes run only after a person approves the card")
	}
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil {
		return nil, err
	}
	if approval.Status != ApprovalStatusApproved || approval.DecisionBy == nil {
		return nil, errors.New("agent builder changes run only after a person approves the card")
	}
	if approval.DecisionBy.Type != "user" || strings.TrimSpace(approval.DecisionBy.ID) == "" {
		return nil, fmt.Errorf("agent builder cards can be approved only by a person, not %s", approval.DecisionBy.Type)
	}
	return approval, nil
}

// agentBuilderResourceID derives a definition or deployment id from the
// call, in the 21-character form of the existing ids.
func agentBuilderResourceID(kind string, scope Scope, callID string) string {
	digest := sha256.Sum256([]byte(kind + ":" + scope.Kind + ":" + scope.ID + ":" + callID))
	return "a" + base64.RawURLEncoding.EncodeToString(digest[:])[:20]
}

func agentBuilderBindingID(deploymentID, skillID string) string {
	return "builder:" + deploymentID + ":" + skillID
}

func (d *AgentBuilderActionDispatcher) listAgents(ctx context.Context, scope Scope, builderID string, arguments map[string]interface{}) (map[string]interface{}, error) {
	var args agentListArguments
	if err := decodeAgentBuilderArguments(arguments, &args); err != nil {
		return nil, err
	}
	query := normalizedAgentName(args.Query)
	deployments, err := d.agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{
		Scope: agentBuilderScope(scope), ExcludeStatuses: []kernelagent.RolloutStatus{kernelagent.RolloutRetired},
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
		if query != "" && !strings.Contains(normalizedAgentName(name), query) &&
			!strings.Contains(normalizedAgentName(definition.Purpose), query) {
			continue
		}
		spaceIDs := memberships[deployment.ID]
		if spaceIDs == nil {
			spaceIDs = []string{}
		}
		agents = append(agents, map[string]interface{}{
			"id": deployment.ID, "displayName": name, "purpose": definition.Purpose,
			"rolloutStatus": deployment.RolloutStatus, "spaceIds": spaceIDs,
			"manageable": deployment.ID != builderID && !agentDefinitionPolicyFixed(definition),
		})
	}
	return agentBuilderJSONMap(map[string]interface{}{"resourceType": "agent_list", "agents": agents})
}

// agentSpaceIDs maps each Agent deployment to the spaces it is in.
func (d *AgentBuilderActionDispatcher) agentSpaceIDs(ctx context.Context, scope Scope) (map[string][]string, error) {
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
			if member != nil && member.Type == SpaceMemberAgent {
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

func (d *AgentBuilderActionDispatcher) createAgent(ctx context.Context, input ActionDispatchInput, run *AgentRun) (map[string]interface{}, error) {
	call := input.Call
	approval, err := approvedAgentBuilderCheckpoint(ctx, d.store, call)
	if err != nil {
		return nil, err
	}
	scope := call.Scope
	reference := agentBuilderScope(scope)
	var args agentCreateArguments
	if err := decodeAgentBuilderArguments(input.Arguments, &args); err != nil {
		return nil, err
	}
	args.agentProfileArguments = args.agentProfileArguments.normalized()
	args.SpaceID = strings.TrimSpace(args.SpaceID)
	if err := validateAgentProfileText(args.agentProfileArguments); err != nil {
		return nil, err
	}
	definitionID := agentBuilderResourceID("agent-definition", scope, call.ID)
	deploymentID := agentBuilderResourceID("agent-deployment", scope, call.ID)
	// The name may have been taken while the card waited. This call's own
	// deployment is excluded so a replay passes.
	if err := ensureAgentNameAvailable(ctx, d.agents, d.teams, scope, args.DisplayName, deploymentID); err != nil {
		return nil, fmt.Errorf("%w. Propose create_agent again with another name", err)
	}
	space, err := d.createSpace(ctx, scope, args.SpaceID)
	if err != nil {
		return nil, err
	}
	grants, err := resolveAgentSkillGrants(ctx, d.catalog, args.Skills)
	if err != nil {
		return nil, err
	}
	approver := approval.DecisionBy.ID
	definition, err := d.registerAgentBuilderDefinition(ctx, definitionID, args.agentProfileArguments, call.ID, approver)
	if err != nil {
		return nil, err
	}
	deployment, activation, replayed, err := d.createAgentDeployment(ctx, reference, deploymentID, definition, approver)
	if err != nil {
		return nil, err
	}
	if err := d.placeAgentInSpace(ctx, scope, space, deploymentID); err != nil {
		return nil, err
	}
	actor := skill.BindingActor{Type: approval.DecisionBy.Type, ID: approver}
	bindings := make([]interface{}, 0, len(grants))
	for _, grant := range grants {
		binding, err := d.grantAgentSkill(ctx, scope, deploymentID, grant.grant, actor, agentBuilderCreateReason)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, secretSafeBindingPreview(binding))
	}
	conversation, err := d.provision.EnsureAgentControlConversation(ctx, deployment)
	if err != nil {
		return nil, err
	}
	if err := d.prepareAgentModel(ctx, AgentModelRequest{
		Scope: reference, DeploymentID: deploymentID, Model: args.Model, OwnerUserID: approver, ActionCallID: call.ID,
	}); err != nil {
		return nil, err
	}
	if d.host != nil {
		d.host.AfterAgentCreated(ctx, AgentCreated{
			Scope: scope, DeploymentID: deploymentID, DisplayName: definition.DisplayName, Purpose: definition.Purpose,
			ActionCallID: call.ID, RunID: run.ID, BillingUserID: approver,
		})
	}
	connections := make([]map[string]interface{}, 0, len(args.Connections))
	for _, connection := range args.Connections {
		connections = append(connections, map[string]interface{}{
			"skillId": strings.TrimSpace(connection.SkillID), "skillVersion": strings.TrimSpace(connection.SkillVersion),
			"sourceIdentity": strings.TrimSpace(connection.SourceIdentity),
		})
	}
	message := fmt.Sprintf("Created %s. Offer 'Chat with %s'.", definition.DisplayName, definition.DisplayName)
	if len(connections) > 0 {
		message += fmt.Sprintf(" For each connection, call openseal.skills.request_connection with targetAgentId=%s.", deploymentID)
	}
	return agentBuilderJSONMap(map[string]interface{}{
		"resourceType": agentProfileResourceType, "operation": AgentActionCreateAgent,
		"deploymentId": deploymentID, "displayName": definition.DisplayName, "replayed": replayed,
		"deployment": deployment, "definition": definition, "activation": activation,
		"bindings": bindings, "space": map[string]interface{}{"id": space.ID, "name": space.Name},
		"connections": connections, "chat": map[string]interface{}{"conversationId": conversation.ID},
		"message": message,
	})
}

// createSpace resolves the reviewed space at execution. A space deleted
// while the card waited fails the call.
func (d *AgentBuilderActionDispatcher) createSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error) {
	if spaceID == "" {
		return nil, errors.New("create_agent requires a space")
	}
	if spaceID == PersonalSpaceID(scope) {
		return &Space{ID: spaceID, Scope: scope, Name: PersonalSpaceName, IsPersonal: true}, nil
	}
	spaces, err := d.provision.ListSpaces(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, space := range spaces {
		if space != nil && space.ID == spaceID {
			return space, nil
		}
	}
	return nil, fmt.Errorf("space %q no longer exists; propose create_agent again", spaceID)
}

// registerAgentBuilderDefinition registers the reviewed profile as version
// 1.0.0 of definitionID. A definition this call already registered counts as
// success.
func (d *AgentBuilderActionDispatcher) registerAgentBuilderDefinition(ctx context.Context, definitionID string, profile agentProfileArguments, callID, approver string) (*kernelagent.AgentDefinition, error) {
	existing, err := d.agents.GetDefinition(ctx, definitionID, agentBuilderDefinitionVersion)
	if err == nil && existing != nil {
		if existing.Provenance.Source != agentBuilderProvenanceSource || existing.Provenance.Reference != callID {
			return nil, fmt.Errorf("agent definition %s already exists for another change", definitionID)
		}
		return existing, nil
	}
	if err != nil && !errors.Is(err, kernelagent.ErrDefinitionNotFound) {
		return nil, err
	}
	definition := &kernelagent.AgentDefinition{
		ID: definitionID, Version: agentBuilderDefinitionVersion,
		DisplayName: profile.DisplayName, Purpose: profile.Purpose, SystemPrompt: profile.SystemPrompt,
		Personality: profile.Personality, OperatingPrinciples: append([]string(nil), profile.OperatingPrinciples...),
		Authority:  kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Amendments: workforce.AmendmentPolicy{AgentMayPropose: true},
		Provenance: workforce.DefinitionProvenance{Source: agentBuilderProvenanceSource, Reference: callID, CreatedBy: approver},
	}
	registered, err := d.agents.RegisterDefinition(ctx, definition)
	if err != nil {
		// A concurrent attempt of the same call may have registered it.
		if existing, getErr := d.agents.GetDefinition(ctx, definitionID, agentBuilderDefinitionVersion); getErr == nil && existing != nil &&
			existing.Provenance.Source == agentBuilderProvenanceSource && existing.Provenance.Reference == callID {
			return existing, nil
		}
		return nil, err
	}
	return registered, nil
}

// createAgentDeployment creates the deployment, or returns the one an
// earlier attempt of the call created (replayed true).
func (d *AgentBuilderActionDispatcher) createAgentDeployment(ctx context.Context, reference capability.ScopeReference, deploymentID string, definition *kernelagent.AgentDefinition, approver string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, bool, error) {
	existing, err := d.agents.GetDeployment(ctx, reference, deploymentID)
	if err == nil && existing != nil {
		if existing.DefinitionID != definition.ID {
			return nil, nil, false, fmt.Errorf("agent deployment %s already exists for another definition", deploymentID)
		}
		activation, err := d.firstAgentActivation(ctx, reference, deploymentID)
		return existing, activation, true, err
	}
	if err != nil && !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		return nil, nil, false, err
	}
	deployment, activation, err := d.provision.CreateAgentDeployment(ctx, &kernelagent.AgentDeployment{
		ID: deploymentID, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default",
		Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1,
	}, "user", approver, agentBuilderCreateReason)
	if err != nil {
		if deployment == nil {
			return nil, nil, false, err
		}
		// The deployment committed; its control conversation is ensured
		// again below.
	}
	return deployment, activation, false, nil
}

func (d *AgentBuilderActionDispatcher) firstAgentActivation(ctx context.Context, reference capability.ScopeReference, deploymentID string) (*kernelagent.DefinitionActivation, error) {
	activations, err := d.agents.ListActivations(ctx, reference, deploymentID)
	if err != nil {
		return nil, err
	}
	if len(activations) == 0 {
		return nil, nil
	}
	first := activations[0]
	for _, activation := range activations[1:] {
		if activation.CreatedAt.Before(first.CreatedAt) {
			first = activation
		}
	}
	return &first, nil
}

// placeAgentInSpace adds the Agent to the reviewed space. Outside Personal it
// then leaves Personal, where the deployment create placed it.
func (d *AgentBuilderActionDispatcher) placeAgentInSpace(ctx context.Context, scope Scope, space *Space, deploymentID string) error {
	if _, _, err := d.provision.PutSpaceMember(ctx, PutSpaceMemberRequest{
		Scope: scope, SpaceID: space.ID, Type: SpaceMemberAgent, MemberID: deploymentID,
	}); err != nil {
		return err
	}
	personal := PersonalSpaceID(scope)
	if space.ID == personal {
		return nil
	}
	if err := d.provision.DeleteSpaceMember(ctx, scope, personal, SpaceMemberAgent, deploymentID); err != nil && !errors.Is(err, ErrSpaceNotFound) {
		return err
	}
	return nil
}

// grantAgentSkill binds one grant under its builder binding id. A binding
// that already matches the grant (an earlier attempt) is returned as is.
func (d *AgentBuilderActionDispatcher) grantAgentSkill(ctx context.Context, scope Scope, deploymentID string, grant AgentSkillGrant, actor skill.BindingActor, reason string) (*skill.Binding, error) {
	reference := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	bindingID := agentBuilderBindingID(deploymentID, grant.SkillID)
	args := skillBindingUpsertArguments{
		BindingID: bindingID, SkillID: grant.SkillID, SkillVersion: grant.SkillVersion, SourceIdentity: grant.SourceIdentity,
		AllowedActions: grant.AllowedActions, EnablePrompt: grant.EnablePrompt, MaximumRisk: grant.MaximumRisk, Config: grant.Config,
	}
	current, err := d.catalog.GetBinding(ctx, reference, deploymentID, bindingID)
	if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
		return nil, err
	}
	candidate := skillBindingCandidate(reference, deploymentID, args, current)
	if current != nil && !current.Disabled && sameManagedSkillBinding(current, candidate) {
		return current, nil
	}
	var expected int64
	if current != nil {
		expected = current.Revision
	}
	binding, err := d.catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{
		Binding: candidate, ExpectedRevision: expected, Actor: actor, Reason: reason,
	})
	if errors.Is(err, skill.ErrBindingRevisionConflict) {
		latest, getErr := d.catalog.GetBinding(ctx, reference, deploymentID, bindingID)
		if getErr == nil && latest != nil && !latest.Disabled && sameManagedSkillBinding(latest, candidate) {
			return latest, nil
		}
	}
	return binding, err
}

func (d *AgentBuilderActionDispatcher) prepareAgentModel(ctx context.Context, request AgentModelRequest) error {
	if d.host == nil {
		return nil
	}
	if err := d.host.PrepareAgentModel(ctx, request); err != nil {
		return fmt.Errorf("prepare the Agent's model: %w", err)
	}
	return nil
}

func (d *AgentBuilderActionDispatcher) updateAgent(ctx context.Context, input ActionDispatchInput, run *AgentRun) (map[string]interface{}, error) {
	call := input.Call
	approval, err := approvedAgentBuilderCheckpoint(ctx, d.store, call)
	if err != nil {
		return nil, err
	}
	scope := call.Scope
	reference := agentBuilderScope(scope)
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	builderID := strings.TrimSpace(run.AssignedAgentID)
	var args agentUpdateArguments
	if err := decodeAgentBuilderArguments(input.Arguments, &args); err != nil {
		return nil, err
	}
	args.AgentID = strings.TrimSpace(args.AgentID)
	if args.AgentID == "" || args.AgentID == builderID {
		return nil, errors.New("update_agent requires another Agent's agentId")
	}
	deployment, err := d.agents.GetDeployment(ctx, reference, args.AgentID)
	if err != nil {
		return nil, agentBuilderTargetError(args.AgentID, err)
	}
	// Removals an earlier attempt already made are done; the rest must
	// still be enabled bindings of the target.
	arguments := cloneMap(input.Arguments)
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
		if len(resolved.changes) > 0 {
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
		binding, err := d.grantAgentSkill(ctx, scope, deployment.ID, grant.grant, actor, agentBuilderUpdateReason)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, secretSafeBindingPreview(binding))
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
			removedBindings = append(removedBindings, secretSafeBindingPreview(binding))
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
		"resourceType": agentProfileResourceType, "operation": AgentActionUpdateAgent,
		"deploymentId": deployment.ID, "displayName": name, "replayed": replayed,
		"deployment": deployment, "bindings": bindings, "removedBindings": removedBindings,
		"message": fmt.Sprintf("Updated %s.", name),
	}
	if amendment != nil {
		result["amendment"] = amendment
	}
	if activation != nil {
		result["activation"] = activation
	}
	return agentBuilderJSONMap(result)
}

func (d *AgentBuilderActionDispatcher) agentActivationTo(ctx context.Context, reference capability.ScopeReference, deploymentID, version string) (*kernelagent.DefinitionActivation, error) {
	activations, err := d.agents.ListActivations(ctx, reference, deploymentID)
	if err != nil {
		return nil, err
	}
	for index := len(activations) - 1; index >= 0; index-- {
		if activations[index].ToVersion == version {
			activation := activations[index]
			return &activation, nil
		}
	}
	return nil, nil
}

// disableAgentSkill disables one binding; an already disabled binding is done.
func (d *AgentBuilderActionDispatcher) disableAgentSkill(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string, actor skill.BindingActor) error {
	if strings.HasPrefix(bindingID, "bundled:") {
		return fmt.Errorf("binding %s is built in and cannot be removed", bindingID)
	}
	current, err := d.catalog.GetBinding(ctx, scope, deploymentID, bindingID)
	if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
		return err
	}
	if current == nil || current.Disabled {
		return nil
	}
	_, err = d.catalog.DisableBinding(ctx, skill.DisableBindingRequest{
		Scope: scope, DeploymentID: deploymentID, BindingID: bindingID, ExpectedRevision: current.Revision,
		Actor: actor, Reason: agentBuilderUpdateReason,
	})
	if errors.Is(err, skill.ErrBindingAlreadyDisabled) {
		return nil
	}
	return err
}
