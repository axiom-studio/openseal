package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

type workforceApplication struct {
	activation                 authoring.WorkforceActivationIntent
	agentDefinitions           []*agent.AgentDefinition
	agentDeployments           []*agent.AgentDeployment
	agentActivations           []workforce.DefinitionActivation
	skillBindings              []*capability.Binding
	teamDefinition             *team.Definition
	teamDeployment             *team.Deployment
	teamActivation             workforce.DefinitionActivation
	objectives                 []workforceObjectiveApplication
	initiative                 *Initiative
	initiativeExpectedRevision int64
	conversationEndpoints      []workforceConversationEndpointApplication
	resources                  []authoring.AppliedResourceReference
}

type workforceObjectiveApplication struct {
	value            *Objective
	expectedRevision int64
}

type workforceConversationEndpointApplication struct {
	value            *ExternalConversationEndpoint
	expectedRevision int64
}

// Authoring mode describes how the model produced the candidate. Persistence
// intent is per resource: revision zero creates, while a positive revision is
// the compare-and-swap boundary for an update. This also scopes destructive
// binding reconciliation to deployments this ChangeSet is actually updating.
func workforceBindingReconciliationDeployments(value *authoring.ChangeSet) map[string]bool {
	deployments := map[string]bool{}
	if value == nil {
		return deployments
	}
	for definitionID, expectedRevision := range value.Placement.AgentExpectedRevisions {
		if expectedRevision > 0 {
			if deploymentID := strings.TrimSpace(value.Placement.AgentDeploymentIDs[definitionID]); deploymentID != "" {
				deployments[deploymentID] = true
			}
		}
	}
	if value.Placement.TeamExpectedRevision > 0 {
		if deploymentID := strings.TrimSpace(value.Placement.TeamDeploymentID); deploymentID != "" {
			deployments[deploymentID] = true
		}
	}
	return deployments
}

func materializeWorkforceApplication(value *authoring.ChangeSet) (*workforceApplication, error) {
	if value == nil || value.ApplyReceipt == nil {
		return nil, fmt.Errorf("applied workforce aggregate is incomplete")
	}
	activation, err := authoring.EffectiveChangeSetActivationIntent(value)
	if err != nil {
		return nil, err
	}
	if value.ApplyReceipt.Activation != "" && value.ApplyReceipt.Activation != activation {
		return nil, fmt.Errorf("apply receipt activation %s does not match reviewed candidate activation %s", value.ApplyReceipt.Activation, activation)
	}
	now, scope := value.ApplyReceipt.AppliedAt, value.Scope
	application := &workforceApplication{activation: activation}
	activate := activation == authoring.WorkforceActivationActive
	deploymentByDefinition := map[string]string{}
	for _, definition := range value.Result.Candidate.Agents {
		if definition != nil {
			deploymentByDefinition[definition.ID] = value.Placement.AgentDeploymentIDs[definition.ID]
		}
	}
	for index, source := range value.Result.Candidate.Agents {
		if source == nil {
			return nil, fmt.Errorf("Agent definition is required")
		}
		definition := cloneJSON(source)
		deploymentID := value.Placement.AgentDeploymentIDs[definition.ID]
		bindings, err := materializeWorkforceSkillBindings(value, definition, deploymentID, activate)
		if err != nil {
			return nil, err
		}
		if err := resolveAgentDefinitionSkillIdentities(value, definition); err != nil {
			return nil, err
		}
		if err := resolveRunbookAgentDeploymentIDs(definition.Runbook, deploymentByDefinition); err != nil {
			return nil, err
		}
		definition.CreatedAt = now
		definition.Digest = ""
		definition.Digest = portableDigest(definition)
		expectedRevision := value.Placement.AgentExpectedRevisions[definition.ID]
		revision := expectedRevision + 1
		previous := ""
		if expectedRevision > 0 {
			previous = "__load__"
		}
		rolloutStatus := agent.RolloutActive
		if !activate {
			rolloutStatus = agent.RolloutPending
			if expectedRevision > 0 {
				rolloutStatus = agent.RolloutPaused
			}
		}
		deployment := &agent.AgentDeployment{ID: deploymentID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, PreviousVersion: previous, RolloutStatus: rolloutStatus, Environment: value.Placement.Environment, Credentials: value.Placement.CredentialReferences[definition.ID], Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: definition.Authority.MaxConcurrentRuns}, Revision: revision, CreatedAt: now, UpdatedAt: now}
		if err := deployment.Validate(); err != nil {
			return nil, err
		}
		activation := workforce.DefinitionActivation{ID: value.ApplyReceipt.ID + fmt.Sprintf(":agent:%d", index), Scope: scope, DeploymentID: deployment.ID, DefinitionID: definition.ID, ToVersion: definition.Version, DeploymentRevision: revision, Reason: "workforce_change_set:" + value.ID, ActorType: value.ApplyReceipt.Actor.Type, ActorID: value.ApplyReceipt.Actor.ID, CreatedAt: now}
		application.agentDefinitions = append(application.agentDefinitions, definition)
		application.agentDeployments = append(application.agentDeployments, deployment)
		application.agentActivations = append(application.agentActivations, activation)
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "agent_definition", ID: definition.ID, Version: definition.Version}, authoring.AppliedResourceReference{Kind: "agent_deployment", ID: deployment.ID, Version: definition.Version, Revision: revision})
		application.skillBindings = append(application.skillBindings, bindings...)
		for _, binding := range bindings {
			application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "skill_binding", ID: binding.ID, Version: binding.SkillVersion, Revision: binding.Revision})
		}
		objectives, err := materializeObjectives(value, "agent", definition.ID, deployment.ID, definition.ObjectiveTemplates, deploymentByDefinition)
		if err != nil {
			return nil, err
		}
		application.objectives = append(application.objectives, objectives...)
	}
	if value.Result.Candidate.Team == nil {
		if err := finishWorkforceApplication(value, application, deploymentByDefinition); err != nil {
			return nil, err
		}
		return application, nil
	}
	definition := cloneJSON(value.Result.Candidate.Team)
	if err := resolveTeamDefinitionSkillIdentities(value, definition); err != nil {
		return nil, err
	}
	definition.CreatedAt = now
	definition.Digest = ""
	definition.Digest = portableDigest(definition)
	application.teamDefinition = definition
	roster := make([]team.RosterAssignment, 0, len(value.Result.Candidate.Assignments))
	for _, assignment := range value.Result.Candidate.Assignments {
		roster = append(roster, team.RosterAssignment{ID: assignment.ID, RoleID: assignment.RoleID, AgentDeploymentID: deploymentByDefinition[assignment.AgentDefinitionID], DisplayName: assignment.DisplayName})
	}
	revision := value.Placement.TeamExpectedRevision + 1
	teamStatus := team.DeploymentActive
	if !activate {
		teamStatus = team.DeploymentDraft
		if value.Placement.TeamExpectedRevision > 0 {
			teamStatus = team.DeploymentPaused
		}
	}
	application.teamDeployment = &team.Deployment{ID: value.Placement.TeamDeploymentID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Roster: roster, Status: teamStatus, Revision: revision, CreatedAt: now, UpdatedAt: now}
	if err := application.teamDeployment.Validate(definition); err != nil {
		return nil, err
	}
	teamBindings, err := materializeWorkforceTeamSkillBindings(value, definition, application.teamDeployment.ID, deploymentByDefinition, application.skillBindings, activate)
	if err != nil {
		return nil, err
	}
	application.skillBindings = append(application.skillBindings, teamBindings...)
	for _, binding := range teamBindings {
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "skill_binding", ID: binding.ID, Version: binding.SkillVersion, Revision: binding.Revision})
	}
	application.teamActivation = workforce.DefinitionActivation{ID: value.ApplyReceipt.ID + ":team", Scope: scope, DeploymentID: application.teamDeployment.ID, DefinitionID: definition.ID, ToVersion: definition.Version, DeploymentRevision: revision, Reason: "workforce_change_set:" + value.ID, ActorType: value.ApplyReceipt.Actor.Type, ActorID: value.ApplyReceipt.Actor.ID, CreatedAt: now}
	application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "team_definition", ID: definition.ID, Version: definition.Version}, authoring.AppliedResourceReference{Kind: "team_deployment", ID: application.teamDeployment.ID, Version: definition.Version, Revision: revision})
	objectives, err := materializeObjectives(value, "team", definition.ID, application.teamDeployment.ID, definition.ObjectiveTemplates, deploymentByDefinition)
	if err != nil {
		return nil, err
	}
	application.objectives = append(application.objectives, objectives...)
	if err := finishWorkforceApplication(value, application, deploymentByDefinition); err != nil {
		return nil, err
	}
	return application, nil
}

func finishWorkforceApplication(value *authoring.ChangeSet, application *workforceApplication, deploymentByDefinition map[string]string) error {
	for _, objective := range application.objectives {
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "objective", ID: objective.value.ID, Revision: objective.value.Revision})
	}
	if value.Result.Candidate.Initiative != nil {
		initiative, err := materializeInitiative(value, application, deploymentByDefinition)
		if err != nil {
			return err
		}
		application.initiative = initiative
		application.initiativeExpectedRevision = value.Placement.InitiativeExpectedRevision
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "initiative", ID: initiative.ID, Revision: initiative.Revision})
	}
	if err := materializeConversationEndpoints(value, application, deploymentByDefinition); err != nil {
		return err
	}
	recordedBindings := map[string]bool{}
	for _, resource := range application.resources {
		if resource.Kind == "skill_binding" {
			recordedBindings[resource.ID] = true
		}
	}
	for _, binding := range application.skillBindings {
		if binding != nil && !recordedBindings[binding.ID] {
			application.resources = append(application.resources, authoring.AppliedResourceReference{
				Kind: "skill_binding", ID: binding.ID, Version: binding.SkillVersion, Revision: binding.Revision,
			})
			recordedBindings[binding.ID] = true
		}
	}
	for _, endpoint := range application.conversationEndpoints {
		application.resources = append(application.resources, authoring.AppliedResourceReference{
			Kind: "conversation_endpoint", ID: endpoint.value.ID, Revision: endpoint.value.Revision,
		})
	}
	sortWorkforceApplicationResources(application)
	return nil
}

func sortWorkforceApplicationResources(application *workforceApplication) {
	if application == nil {
		return
	}
	sort.Slice(application.resources, func(i, j int) bool {
		return application.resources[i].Kind+application.resources[i].ID < application.resources[j].Kind+application.resources[j].ID
	})
}

func materializeConversationEndpoints(
	value *authoring.ChangeSet,
	application *workforceApplication,
	deploymentByDefinition map[string]string,
) error {
	if value == nil || application == nil {
		return nil
	}
	activate := application.activation == authoring.WorkforceActivationActive
	for _, blueprint := range value.Result.Candidate.ConversationEndpoints {
		placement, ok := value.Placement.ConversationEndpoints[blueprint.ID]
		if !ok {
			return fmt.Errorf("conversation endpoint %s has no reviewed placement", blueprint.ID)
		}
		ownerDeploymentID := ""
		ownerType := OwnerTypeAgent
		switch blueprint.Owner.Type {
		case authoring.ConversationEndpointOwnerAgent:
			ownerDeploymentID = deploymentByDefinition[blueprint.Owner.ID]
		case authoring.ConversationEndpointOwnerTeam:
			ownerType = OwnerTypeTeam
			if application.teamDeployment != nil && value.Result.Candidate.Team != nil &&
				value.Result.Candidate.Team.ID == blueprint.Owner.ID {
				ownerDeploymentID = application.teamDeployment.ID
			}
		}
		if strings.TrimSpace(ownerDeploymentID) == "" {
			return fmt.Errorf("conversation endpoint %s owner has no deployment", blueprint.ID)
		}
		skillCapability, ok := value.Catalog.Skills[blueprint.SkillID]
		if !ok || skillCapability.Version != blueprint.SkillVersion {
			return fmt.Errorf("conversation endpoint %s Skill adapter is unavailable", blueprint.ID)
		}
		var adapter *authoring.ConversationAdapterCapability
		for index := range skillCapability.ConversationAdapters {
			if skillCapability.ConversationAdapters[index].ID == blueprint.AdapterID {
				adapter = &skillCapability.ConversationAdapters[index]
				break
			}
		}
		if adapter == nil {
			return fmt.Errorf("conversation endpoint %s adapter is unavailable", blueprint.ID)
		}
		binding, err := materializeConversationAdapterBinding(
			value, application, blueprint, ownerDeploymentID, adapter, activate,
		)
		if err != nil {
			return err
		}
		handler := ExternalConversationHandler{}
		switch blueprint.Handler.Kind {
		case authoring.ConversationHandlerAgent:
			handler = ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: ownerDeploymentID}
		case authoring.ConversationHandlerTeam:
			handler = ExternalConversationHandler{Kind: ExternalConversationHandlerTeam, ID: ownerDeploymentID}
		case authoring.ConversationHandlerRunbook:
			assignedAgentID := deploymentByDefinition[blueprint.Handler.AgentDefinitionID]
			if strings.TrimSpace(assignedAgentID) == "" {
				return fmt.Errorf("conversation endpoint %s Runbook Agent has no deployment", blueprint.ID)
			}
			handler = ExternalConversationHandler{
				Kind: ExternalConversationHandlerRunbook, ID: blueprint.Handler.RunbookID,
				Version: blueprint.Handler.RunbookVersion, Trigger: blueprint.Handler.Trigger,
				AssignedAgentID: assignedAgentID,
			}
		default:
			return fmt.Errorf("conversation endpoint %s handler is invalid", blueprint.ID)
		}
		status := ExternalConversationEndpointPaused
		if activate {
			status = ExternalConversationEndpointActive
		}
		address := strings.TrimSpace(placement.Address)
		if address == "" {
			address = strings.TrimSpace(blueprint.Address)
		}
		now := value.ApplyReceipt.AppliedAt
		endpoint := &ExternalConversationEndpoint{
			ID: strings.TrimSpace(placement.ID), IngressRoute: uuid.NewString(),
			Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID},
			Owner: ObjectiveOwner{Type: ownerType, ID: ownerDeploymentID}, DeploymentID: ownerDeploymentID,
			Name: blueprint.Name,
			Adapter: ExternalConversationAdapterReference{
				SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity,
				BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: blueprint.AdapterID,
			},
			Provider: adapter.Provider, Mode: blueprint.Mode, Address: address, Handler: handler,
			Policy: ExternalConversationPolicy{
				MessageSelection: ExternalConversationMessageSelection(blueprint.Policy.MessageSelection),
				ReplyMode:        ExternalConversationReplyMode(blueprint.Policy.ReplyMode),
				IgnoreBots:       blueprint.Policy.IgnoreBots,
			},
			Configuration: cloneMap(placement.Configuration), Status: status,
			Revision: placement.ExpectedRevision + 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := endpoint.Policy.Validate(endpoint.Mode, adapter.Features); err != nil {
			return err
		}
		if err := endpoint.Validate(); err != nil {
			return fmt.Errorf("conversation endpoint %s is invalid: %w", blueprint.ID, err)
		}
		application.conversationEndpoints = append(application.conversationEndpoints, workforceConversationEndpointApplication{
			value: endpoint, expectedRevision: placement.ExpectedRevision,
		})
	}
	return nil
}

func materializeConversationAdapterBinding(
	value *authoring.ChangeSet,
	application *workforceApplication,
	blueprint authoring.ConversationEndpointBlueprint,
	deploymentID string,
	adapter *authoring.ConversationAdapterCapability,
	activate bool,
) (*capability.Binding, error) {
	identity := workforceSkillRuntimeIdentity(
		value, blueprint.Owner.ID, blueprint.SkillID, blueprint.SkillVersion,
	)
	bindingID := "workforce:" + deploymentID + ":" + blueprint.SkillID
	var binding *capability.Binding
	for _, candidate := range application.skillBindings {
		if candidate.ID == bindingID {
			binding = candidate
			break
		}
	}
	if binding == nil {
		binding = &capability.Binding{
			ID: bindingID, Scope: value.Scope, DeploymentID: deploymentID,
			SkillID: identity.ID, SkillVersion: identity.Version, SourceIdentity: identity.SourceIdentity,
			AllowedActions: []string{}, MaximumRisk: capability.RiskLevelRead,
			Credentials: map[string]capability.CredentialReference{},
			Config:      cloneMap(value.Placement.BindingConfigs[blueprint.Owner.ID][blueprint.SkillID]),
			Disabled:    !activate, Revision: 1,
		}
		application.skillBindings = append(application.skillBindings, binding)
	} else if binding.SkillID != identity.ID || binding.SkillVersion != identity.Version ||
		binding.SourceIdentity != identity.SourceIdentity {
		return nil, fmt.Errorf("conversation endpoint %s conflicts with the owner's exact Skill binding", blueprint.ID)
	}
	if !containsExactRuntimeString(binding.EnabledConversationAdapters, blueprint.AdapterID) {
		binding.EnabledConversationAdapters = append(binding.EnabledConversationAdapters, blueprint.AdapterID)
		sort.Strings(binding.EnabledConversationAdapters)
	}
	for _, credential := range adapter.Credentials {
		reference := value.Placement.CredentialReferences[blueprint.Owner.ID][credential.Name]
		if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
			if credential.Optional || !activate {
				continue
			}
			return nil, fmt.Errorf(
				"conversation endpoint %s requires opaque credential %s of kind %s",
				blueprint.ID, credential.Name, credential.Kind,
			)
		}
		if reference.Kind != credential.Kind {
			return nil, fmt.Errorf(
				"conversation endpoint %s credential %s must use kind %s",
				blueprint.ID, credential.Name, credential.Kind,
			)
		}
		binding.Credentials[credential.Name] = reference
	}
	if err := skill.ValidateBindingShape(binding); err != nil {
		return nil, fmt.Errorf("conversation endpoint %s Skill binding is invalid: %w", blueprint.ID, err)
	}
	return binding, nil
}

func resolveRunbookAgentDeploymentIDs(definition *runbook.Definition, deployments map[string]string) error {
	if definition == nil {
		return nil
	}
	for stepID, step := range definition.Steps {
		if step.Delegate == nil || len(step.Delegate.AgentID.Literal) == 0 {
			continue
		}
		var agentID string
		if err := json.Unmarshal(step.Delegate.AgentID.Literal, &agentID); err != nil || strings.TrimSpace(agentID) == "" {
			continue
		}
		deploymentID := strings.TrimSpace(deployments[agentID])
		if deploymentID == "" {
			continue
		}
		encoded, _ := json.Marshal(deploymentID)
		step.Delegate.AgentID.Literal = encoded
		definition.Steps[stepID] = step
	}
	return nil
}

func materializeWorkforceSkillBindings(value *authoring.ChangeSet, definition *agent.AgentDefinition, deploymentID string, activate bool) ([]*capability.Binding, error) {
	bindings := make([]*capability.Binding, 0, len(definition.SkillRequirements))
	for _, requirement := range definition.SkillRequirements {
		skillCapability, ok := value.Catalog.Skills[requirement.SkillID]
		if !ok || strings.TrimSpace(skillCapability.Version) == "" {
			return nil, fmt.Errorf("Skill %s has no immutable catalog version", requirement.SkillID)
		}
		allowed := append([]string(nil), requirement.RequiredActions...)
		sort.Strings(allowed)
		allowedSet := map[string]bool{}
		for _, action := range allowed {
			allowedSet[action] = true
		}
		credentials := map[string]capability.CredentialReference{}
		for _, credential := range skillCapability.Credentials {
			needed := false
			for _, action := range credential.Actions {
				if allowedSet[action] {
					needed = true
					break
				}
			}
			if !needed {
				continue
			}
			bindingKey := strings.TrimSpace(credential.Name)
			if bindingKey == "" {
				bindingKey = strings.TrimSpace(credential.Kind)
			}
			reference := value.Placement.CredentialReferences[definition.ID][bindingKey]
			if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
				if credential.Optional || !activate {
					continue
				}
				return nil, fmt.Errorf("Agent %s Skill %s requires opaque credential %s of kind %s", definition.ID, requirement.SkillID, credential.Name, credential.Kind)
			}
			credentials[credential.Name] = reference
		}
		maximumRisk := skillCapability.MaximumRisk
		if maximumRisk == "" && requirement.PromptRequired && len(allowed) == 0 {
			maximumRisk = capability.RiskLevelRead
		}
		if workforceRiskRank(definition.Authority.MaximumRisk) < workforceRiskRank(maximumRisk) {
			maximumRisk = definition.Authority.MaximumRisk
		}
		identity := workforceSkillRuntimeIdentity(value, definition.ID, requirement.SkillID, skillCapability.Version)
		binding := &capability.Binding{
			ID: "workforce:" + deploymentID + ":" + requirement.SkillID, Scope: value.Scope, DeploymentID: deploymentID,
			SkillID: identity.ID, SkillVersion: identity.Version, SourceIdentity: identity.SourceIdentity, AllowedActions: allowed,
			Disabled: !activate, EnablePrompt: requirement.PromptRequired, MaximumRisk: maximumRisk, Credentials: credentials,
			Config: cloneMap(value.Placement.BindingConfigs[definition.ID][requirement.SkillID]), Revision: 1,
		}
		if err := skill.ValidateBindingShape(binding); err != nil {
			return nil, fmt.Errorf("Agent %s Skill %s authority is invalid: %w", definition.ID, requirement.SkillID, err)
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

type workforceTeamBindingAggregate struct {
	identity     capability.SkillIdentity
	catalogIDs   map[string]bool
	actions      map[string]bool
	enablePrompt bool
	maximumRisk  capability.RiskLevel
	credentials  map[string]capability.CredentialReference
	config       map[string]interface{}
}

// materializeWorkforceTeamSkillBindings creates the Team-owned authority that
// role grants narrow at runtime. The source Agent bindings are independent
// resources; they are used only to prove that the same reviewed placement,
// credentials, and non-secret configuration back every roster member granted
// this exact Skill variant.
func materializeWorkforceTeamSkillBindings(value *authoring.ChangeSet, definition *team.Definition, deploymentID string, deploymentByDefinition map[string]string, agentBindings []*capability.Binding, activate bool) ([]*capability.Binding, error) {
	if value == nil || definition == nil {
		return nil, nil
	}
	bindingsByDeployment := make(map[string]map[string]*capability.Binding)
	for _, binding := range agentBindings {
		if binding == nil || binding.DeploymentID == deploymentID {
			continue
		}
		identity := capability.NewSkillIdentity(binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		if !identity.Valid() {
			continue
		}
		if bindingsByDeployment[binding.DeploymentID] == nil {
			bindingsByDeployment[binding.DeploymentID] = map[string]*capability.Binding{}
		}
		bindingsByDeployment[binding.DeploymentID][identity.Key()] = binding
	}
	assignmentsByRole := make(map[string][]string)
	for _, assignment := range value.Result.Candidate.Assignments {
		if assigned := strings.TrimSpace(deploymentByDefinition[assignment.AgentDefinitionID]); assigned != "" {
			assignmentsByRole[assignment.RoleID] = append(assignmentsByRole[assignment.RoleID], assigned)
		}
	}
	aggregates := make(map[string]*workforceTeamBindingAggregate)
	for _, role := range definition.Roles {
		for _, grant := range role.SkillGrants {
			identity := grant.ExactIdentity()
			if !identity.Valid() {
				return nil, fmt.Errorf("Team role %s Skill grant has no exact runtime identity", role.ID)
			}
			var source *capability.Binding
			for _, assignedDeploymentID := range assignmentsByRole[role.ID] {
				candidate := bindingsByDeployment[assignedDeploymentID][identity.Key()]
				if candidate == nil {
					return nil, fmt.Errorf("Team role %s Agent %s has no reviewed binding for exact Skill %s", role.ID, assignedDeploymentID, identity)
				}
				if source == nil {
					source = candidate
					continue
				}
				if !reflect.DeepEqual(source.Credentials, candidate.Credentials) || !reflect.DeepEqual(source.Config, candidate.Config) {
					return nil, fmt.Errorf("Team role %s exact Skill %s has conflicting roster binding configuration", role.ID, identity)
				}
			}
			if source == nil {
				return nil, fmt.Errorf("Team role %s exact Skill %s has no assigned roster binding", role.ID, identity)
			}
			authorizedActions := make(map[string]bool, len(source.AllowedActions))
			for _, action := range source.AllowedActions {
				authorizedActions[action] = true
			}
			for _, action := range grant.AllowedActions {
				if !authorizedActions[action] {
					return nil, fmt.Errorf("Team role %s action %s exceeds its reviewed Agent binding for Skill %s", role.ID, action, identity)
				}
			}
			if workforceRiskRank(grant.MaximumRisk) > workforceRiskRank(source.MaximumRisk) {
				return nil, fmt.Errorf("Team role %s risk exceeds its reviewed Agent binding for Skill %s", role.ID, identity)
			}
			aggregate := aggregates[identity.Key()]
			if aggregate == nil {
				aggregate = &workforceTeamBindingAggregate{
					identity: identity, catalogIDs: map[string]bool{}, actions: map[string]bool{},
					credentials: cloneCredentialReferences(source.Credentials), config: cloneMap(source.Config),
				}
				aggregates[identity.Key()] = aggregate
			} else if !reflect.DeepEqual(aggregate.credentials, source.Credentials) || !reflect.DeepEqual(aggregate.config, source.Config) {
				return nil, fmt.Errorf("Team exact Skill %s has conflicting role binding configuration", identity)
			}
			catalogID := strings.TrimSpace(grant.CatalogID)
			if catalogID == "" {
				catalogID = strings.TrimSpace(grant.SkillID)
			}
			aggregate.catalogIDs[catalogID] = true
			for _, action := range grant.AllowedActions {
				aggregate.actions[action] = true
			}
			aggregate.enablePrompt = aggregate.enablePrompt || grant.EnablePrompt
			if workforceRiskRank(grant.MaximumRisk) > workforceRiskRank(aggregate.maximumRisk) {
				aggregate.maximumRisk = grant.MaximumRisk
			}
		}
	}
	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	catalogUse := map[string]int{}
	for _, key := range keys {
		for catalogID := range aggregates[key].catalogIDs {
			catalogUse[catalogID]++
		}
	}
	result := make([]*capability.Binding, 0, len(keys))
	for _, key := range keys {
		aggregate := aggregates[key]
		catalogIDs := make([]string, 0, len(aggregate.catalogIDs))
		for catalogID := range aggregate.catalogIDs {
			catalogIDs = append(catalogIDs, catalogID)
		}
		sort.Strings(catalogIDs)
		catalogID := catalogIDs[0]
		bindingID := "workforce:" + deploymentID + ":" + catalogID
		if catalogUse[catalogID] > 1 {
			bindingID += ":" + portableDigest(aggregate.identity)[7:19]
		}
		actions := make([]string, 0, len(aggregate.actions))
		for action := range aggregate.actions {
			actions = append(actions, action)
		}
		sort.Strings(actions)
		result = append(result, &capability.Binding{
			ID: bindingID, Scope: value.Scope, DeploymentID: deploymentID,
			SkillID: aggregate.identity.ID, SkillVersion: aggregate.identity.Version, SourceIdentity: aggregate.identity.SourceIdentity,
			AllowedActions: actions, Disabled: !activate, EnablePrompt: aggregate.enablePrompt, MaximumRisk: aggregate.maximumRisk,
			Credentials: cloneCredentialReferences(aggregate.credentials), Config: cloneMap(aggregate.config), Revision: 1,
		})
	}
	return result, nil
}

func workforceSkillRuntimeIdentity(value *authoring.ChangeSet, agentID, catalogID, catalogVersion string) capability.SkillIdentity {
	if value != nil {
		if identity := value.Placement.SkillRuntimeIdentities[agentID][catalogID].Normalized(); identity.Valid() {
			return identity
		}
		return capability.NewSkillIdentity(catalogID, workforceSkillBindingVersion(value, agentID, catalogID, catalogVersion), strings.TrimSpace(value.Placement.SkillSourceIdentities[agentID][catalogID]))
	}
	return capability.NewSkillIdentity(catalogID, catalogVersion, "")
}

func resolveAgentDefinitionSkillIdentities(value *authoring.ChangeSet, definition *agent.AgentDefinition) error {
	if value == nil || definition == nil || len(value.Placement.SkillRuntimeIdentities[definition.ID]) == 0 {
		return nil
	}
	aliases := make(map[string]string, len(definition.SkillRequirements))
	seen := make(map[string]bool, len(definition.SkillRequirements))
	for index := range definition.SkillRequirements {
		catalogID := strings.TrimSpace(definition.SkillRequirements[index].SkillID)
		identity := value.Placement.SkillRuntimeIdentities[definition.ID][catalogID].Normalized()
		if !identity.Valid() {
			return fmt.Errorf("Agent %s Skill %s has no exact runtime identity", definition.ID, catalogID)
		}
		if seen[identity.ID] {
			return fmt.Errorf("Agent %s selects multiple catalog Skills with runtime id %s", definition.ID, identity.ID)
		}
		seen[identity.ID] = true
		aliases[catalogID] = identity.ID
		definition.SkillRequirements[index].SkillID = identity.ID
	}
	for index, allowed := range definition.Authority.AllowedSkillIDs {
		if canonical := aliases[strings.TrimSpace(allowed)]; canonical != "" {
			definition.Authority.AllowedSkillIDs[index] = canonical
		}
	}
	return definition.Validate()
}

func resolveTeamDefinitionSkillIdentities(value *authoring.ChangeSet, definition *team.Definition) error {
	if value == nil || definition == nil {
		return nil
	}
	assignmentsByRole := make(map[string][]string)
	for _, assignment := range value.Result.Candidate.Assignments {
		assignmentsByRole[assignment.RoleID] = append(assignmentsByRole[assignment.RoleID], assignment.AgentDefinitionID)
	}
	for roleIndex := range definition.Roles {
		role := &definition.Roles[roleIndex]
		resolvedRequired := make([]string, 0, len(role.RequiredSkillIDs))
		for _, catalogID := range role.RequiredSkillIDs {
			ids := resolvedRoleSkillIDs(value, assignmentsByRole[role.ID], strings.TrimSpace(catalogID), "")
			if len(ids) == 0 {
				ids = []string{strings.TrimSpace(catalogID)}
			}
			resolvedRequired = append(resolvedRequired, ids...)
		}
		role.RequiredSkillIDs = uniqueSortedStrings(resolvedRequired)
		resolvedGrants := make([]team.RoleSkillGrant, 0, len(role.SkillGrants))
		for _, grant := range role.SkillGrants {
			if grant.RuntimeIdentity != nil {
				resolvedGrants = append(resolvedGrants, grant)
				continue
			}
			catalogID := strings.TrimSpace(grant.CatalogID)
			if catalogID == "" {
				catalogID = strings.TrimSpace(grant.SkillID)
			}
			identities := resolvedRoleSkillIdentities(value, assignmentsByRole[role.ID], catalogID, grant.SkillID, grant.SkillVersion)
			if len(identities) == 0 {
				resolvedGrants = append(resolvedGrants, grant)
				continue
			}
			for _, identity := range identities {
				copy := grant
				copy.CatalogID, copy.SkillID = catalogID, identity.ID
				copy.RuntimeIdentity = &identity
				resolvedGrants = append(resolvedGrants, copy)
			}
		}
		role.SkillGrants = resolvedGrants
	}
	return definition.Validate()
}

func resolvedRoleSkillIDs(value *authoring.ChangeSet, agentIDs []string, catalogID, declaredVersion string) []string {
	identities := resolvedRoleSkillIdentities(value, agentIDs, catalogID, "", declaredVersion)
	ids := make([]string, 0, len(identities))
	for _, identity := range identities {
		ids = append(ids, identity.ID)
	}
	return uniqueSortedStrings(ids)
}

func resolvedRoleSkillIdentities(value *authoring.ChangeSet, agentIDs []string, catalogID, declaredID, declaredVersion string) []capability.SkillIdentity {
	byKey := map[string]capability.SkillIdentity{}
	for _, agentID := range agentIDs {
		for selectionID, identity := range value.Placement.SkillRuntimeIdentities[agentID] {
			identity = identity.Normalized()
			catalogMatch := strings.TrimSpace(selectionID) == catalogID
			declaredMatch := declaredID != "" && identity.ID == strings.TrimSpace(declaredID) &&
				(declaredVersion == "" || strings.TrimSpace(value.Catalog.Skills[selectionID].Version) == strings.TrimSpace(declaredVersion))
			if identity.Valid() && (catalogMatch || declaredMatch) {
				byKey[identity.Key()] = identity
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]capability.SkillIdentity, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func workforceSkillBindingVersion(value *authoring.ChangeSet, agentID, skillID, catalogVersion string) string {
	if value != nil {
		if version := strings.TrimSpace(value.Placement.SkillSourceVersions[agentID][skillID]); version != "" {
			return version
		}
	}
	return catalogVersion
}

func workforceRiskRank(risk capability.RiskLevel) int {
	switch risk {
	case capability.RiskLevelRead:
		return 1
	case capability.RiskLevelWrite:
		return 2
	case capability.RiskLevelExternal:
		return 3
	case capability.RiskLevelProduction:
		return 4
	case capability.RiskLevelDestructive:
		return 5
	default:
		return 0
	}
}

func validateWorkforceBindingDefinition(binding *capability.Binding, definition *capability.Definition) error {
	if binding == nil {
		return fmt.Errorf("Skill binding is required")
	}
	if definition == nil {
		return fmt.Errorf("Skill binding %s does not match an installed immutable Skill definition", binding.ID)
	}
	if issue := workforceBindingDefinitionIssue("skillBindings."+binding.ID, binding.DeploymentID, binding.SkillID, binding, definition); issue != nil {
		return fmt.Errorf("%s: %s", issue.Code, issue.Message)
	}
	return nil
}

func synchronizeWorkforceSkillBindingResources(application *workforceApplication) {
	if application == nil {
		return
	}
	bindings := map[string]*capability.Binding{}
	for _, binding := range application.skillBindings {
		bindings[binding.ID] = binding
	}
	for index := range application.resources {
		resource := &application.resources[index]
		if resource.Kind == "skill_binding" && bindings[resource.ID] != nil {
			resource.Version = bindings[resource.ID].SkillVersion
			resource.Revision = bindings[resource.ID].Revision
		}
	}
}

func synchronizeWorkforceConversationEndpointBindings(application *workforceApplication) error {
	if application == nil {
		return nil
	}
	bindings := make(map[string]*capability.Binding, len(application.skillBindings))
	for _, binding := range application.skillBindings {
		if binding != nil {
			bindings[binding.ID] = binding
		}
	}
	for index := range application.conversationEndpoints {
		endpoint := application.conversationEndpoints[index].value
		binding := bindings[endpoint.Adapter.BindingID]
		if binding == nil || binding.Revision < 1 ||
			binding.SkillID != endpoint.Adapter.SkillID ||
			binding.SkillVersion != endpoint.Adapter.SkillVersion ||
			binding.SourceIdentity != endpoint.Adapter.SourceIdentity ||
			!containsExactRuntimeString(binding.EnabledConversationAdapters, endpoint.Adapter.AdapterID) {
			return fmt.Errorf("conversation endpoint %s lost its exact Skill adapter binding", endpoint.ID)
		}
		endpoint.Adapter.BindingRevision = binding.Revision
		if err := endpoint.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func containsExactRuntimeString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func materializeObjectives(value *authoring.ChangeSet, ownerType, definitionID, ownerID string, templates []workforce.ObjectiveTemplate, deploymentByDefinition map[string]string) ([]workforceObjectiveApplication, error) {
	result := make([]workforceObjectiveApplication, 0, len(templates))
	activation, err := authoring.EffectiveChangeSetActivationIntent(value)
	if err != nil {
		return nil, err
	}
	status := ObjectiveStatusActive
	if activation == authoring.WorkforceActivationInactive {
		status = ObjectiveStatusDraft
	}
	for _, template := range templates {
		var cadence *ObjectiveCadence
		if len(template.Cadence) > 0 {
			payload, err := json.Marshal(template.Cadence)
			if err != nil {
				return nil, fmt.Errorf("materialize Objective %s cadence: %w", template.ID, err)
			}
			var decoded ObjectiveCadence
			if err := json.Unmarshal(payload, &decoded); err != nil {
				return nil, fmt.Errorf("materialize Objective %s cadence: %w", template.ID, err)
			}
			if deployed := deploymentByDefinition[decoded.AssignedAgentID]; deployed != "" {
				decoded.AssignedAgentID = deployed
			}
			if blueprint := value.Result.Candidate.Initiative; blueprint != nil && decoded.RunTemplate != nil && decoded.RunTemplate.Context["initiativeId"] == blueprint.ID {
				decoded.RunTemplate.Context["initiativeId"] = value.Placement.InitiativeID
			}
			cadence = &decoded
		}
		eventRules, err := materializeObjectiveEventRules(value, template, deploymentByDefinition)
		if err != nil {
			return nil, err
		}
		key := authoring.WorkforceObjectiveKey(ownerType, definitionID, template.ID)
		placement := value.Placement.Objectives[key]
		revision := int64(1)
		if placement.ExpectedRevision > 0 {
			revision = placement.ExpectedRevision + 1
		}
		objective := &Objective{ID: placement.ID, Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Owner: ObjectiveOwner{Type: OwnerType(ownerType), ID: ownerID}, Title: template.Title, Goal: template.Goal, Status: status, Priority: template.Priority, Cadence: cadence, EventRules: eventRules, Constraints: template.Constraints, SuccessCriteria: template.SuccessCriteria, Revision: revision, CreatedAt: value.ApplyReceipt.AppliedAt, UpdatedAt: value.ApplyReceipt.AppliedAt}
		if err := objective.Validate(); err != nil {
			return nil, fmt.Errorf("materialize Objective %s: %w", template.ID, err)
		}
		result = append(result, workforceObjectiveApplication{value: objective, expectedRevision: placement.ExpectedRevision})
	}
	return result, nil
}

func materializeObjectiveEventRules(value *authoring.ChangeSet, template workforce.ObjectiveTemplate, deploymentByDefinition map[string]string) (map[string]interface{}, error) {
	if len(template.EventRules) == 0 {
		return template.EventRules, nil
	}
	rules, err := DecodeObjectiveEventRules(template.EventRules)
	if err != nil {
		return nil, fmt.Errorf("materialize Objective %s event rules: %w", template.ID, err)
	}
	for index := range rules.Rules {
		rule := &rules.Rules[index]
		if deployed := deploymentByDefinition[rule.AssignedAgentID]; deployed != "" {
			rule.AssignedAgentID = deployed
		}
		if blueprint := value.Result.Candidate.Initiative; blueprint != nil && rule.RunTemplate != nil && rule.RunTemplate.Context["initiativeId"] == blueprint.ID {
			rule.RunTemplate.Context["initiativeId"] = value.Placement.InitiativeID
		}
	}
	payload, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("materialize Objective %s event rules: %w", template.ID, err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("materialize Objective %s event rules: %w", template.ID, err)
	}
	return result, nil
}

func materializeInitiative(value *authoring.ChangeSet, application *workforceApplication, deploymentByDefinition map[string]string) (*Initiative, error) {
	blueprint := value.Result.Candidate.Initiative
	if blueprint == nil {
		return nil, nil
	}
	objectiveIDs := make(map[string]string, len(value.Placement.Objectives))
	for key, placement := range value.Placement.Objectives {
		objectiveIDs[key] = placement.ID
	}
	translate := func(refs []string) ([]string, error) {
		result := make([]string, len(refs))
		for index, reference := range refs {
			result[index] = objectiveIDs[reference]
			if result[index] == "" {
				return nil, fmt.Errorf("Initiative Objective reference %s has no placement", reference)
			}
		}
		return result, nil
	}
	ownerID := ""
	switch blueprint.Owner.Type {
	case authoring.InitiativeOwnerAgent:
		ownerID = deploymentByDefinition[blueprint.Owner.DefinitionID]
	case authoring.InitiativeOwnerTeam:
		if application.teamDeployment != nil && application.teamDefinition.ID == blueprint.Owner.DefinitionID {
			ownerID = application.teamDeployment.ID
		}
	}
	if ownerID == "" {
		return nil, fmt.Errorf("Initiative owner has no deployed placement")
	}
	objectiveRefs, err := translate(blueprint.ObjectiveRefs)
	if err != nil {
		return nil, err
	}
	now := value.ApplyReceipt.AppliedAt
	activation, err := authoring.EffectiveChangeSetActivationIntent(value)
	if err != nil {
		return nil, err
	}
	status := InitiativeStatusActive
	if activation == authoring.WorkforceActivationInactive {
		status = InitiativeStatusDraft
	}
	initiative := &Initiative{
		ID: value.Placement.InitiativeID, Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Title: blueprint.Title, Purpose: blueprint.Purpose,
		Status: status, Owner: ObjectiveOwner{Type: OwnerType(blueprint.Owner.Type), ID: ownerID}, ObjectiveRefs: objectiveRefs,
		Policy: cloneMap(blueprint.Policy), Revision: value.Placement.InitiativeExpectedRevision + 1, CreatedAt: now, UpdatedAt: now,
	}
	for index, definition := range application.agentDefinitions {
		deployment := application.agentDeployments[index]
		initiative.AgentRefs = append(initiative.AgentRefs,
			ResourceReference{Kind: ResourceKindAgentDefinition, ID: definition.ID, Version: definition.Version},
			ResourceReference{Kind: ResourceKindAgentDeployment, ID: deployment.ID, Version: definition.Version, Revision: deployment.Revision},
		)
	}
	if application.teamDefinition != nil {
		initiative.TeamRefs = append(initiative.TeamRefs,
			ResourceReference{Kind: ResourceKindTeamDefinition, ID: application.teamDefinition.ID, Version: application.teamDefinition.Version},
			ResourceReference{Kind: ResourceKindTeamDeployment, ID: application.teamDeployment.ID, Version: application.teamDefinition.Version, Revision: application.teamDeployment.Revision},
		)
	}
	for _, source := range blueprint.Milestones {
		refs, err := translate(source.ObjectiveRefs)
		if err != nil {
			return nil, err
		}
		initiative.Milestones = append(initiative.Milestones, InitiativeMilestone{ID: source.ID, Title: source.Title, Status: MilestonePending, ObjectiveRefs: refs})
	}
	for _, source := range blueprint.Hypotheses {
		initiative.Hypotheses = append(initiative.Hypotheses, InitiativeHypothesis{ID: source.ID, Statement: source.Statement, Confidence: source.Confidence, Status: HypothesisOpen, UpdatedAt: now})
	}
	for _, source := range blueprint.SourceMonitors {
		objectiveID := objectiveIDs[source.ObjectiveRef]
		agentID := deploymentByDefinition[source.AssignedAgentDefinitionID]
		if objectiveID == "" || agentID == "" {
			return nil, fmt.Errorf("Initiative source monitor %s has unresolved Objective or Agent placement", source.ID)
		}
		initiative.SourceMonitors = append(initiative.SourceMonitors, SourceMonitorReference{
			ID: source.ID, ObjectiveID: objectiveID, AssignedAgentID: agentID, SkillID: source.SkillID, SkillVersion: source.SkillVersion,
			Action: source.Action, SourcePolicyRef: source.SourcePolicyRef, Deduplication: SourceMonitorDeduplication(source.Deduplication),
		})
	}
	for _, source := range blueprint.Deliverables {
		refs, err := translate(source.ObjectiveRefs)
		if err != nil {
			return nil, err
		}
		initiative.Deliverables = append(initiative.Deliverables, InitiativeDeliverable{ID: source.ID, Title: source.Title, Status: DeliverablePlanned, ObjectiveRefs: refs})
	}
	if err := initiative.Validate(); err != nil {
		return nil, fmt.Errorf("materialize Initiative: %w", err)
	}
	if err := validateMaterializedInitiativeMonitors(initiative, application.objectives); err != nil {
		return nil, err
	}
	idempotency := sha256.Sum256([]byte("workforce-change-set\x00" + value.ID + "\x00" + value.ApplyReceipt.IdempotencyKey))
	initiative.IdempotencyKeyHash = hex.EncodeToString(idempotency[:])
	fingerprint, err := initiativeCreationFingerprint(initiative)
	if err != nil {
		return nil, fmt.Errorf("fingerprint Initiative: %w", err)
	}
	initiative.CreationFingerprint = fingerprint
	return initiative, nil
}

func validateMaterializedInitiativeMonitors(initiative *Initiative, objectives []workforceObjectiveApplication) error {
	byID := make(map[string]*Objective, len(objectives))
	for _, objective := range objectives {
		byID[objective.value.ID] = objective.value
	}
	for _, monitor := range initiative.SourceMonitors {
		objective := byID[monitor.ObjectiveID]
		if objective == nil || objective.Owner != initiative.Owner || objective.Cadence == nil || objective.Cadence.RunTemplate == nil || objective.Cadence.RunTemplate.Capability == nil {
			return fmt.Errorf("Initiative source monitor %s has no matching owned executable Objective", monitor.ID)
		}
		capability := objective.Cadence.RunTemplate.Capability
		if objective.Cadence.AssignedAgentID != monitor.AssignedAgentID || capability.SkillID != monitor.SkillID || capability.SkillVersion != monitor.SkillVersion || capability.Action != monitor.Action ||
			objective.Cadence.RunTemplate.Context["initiativeId"] != initiative.ID || objective.Cadence.RunTemplate.Context["sourceMonitorId"] != monitor.ID ||
			objective.Cadence.RunTemplate.Policy["sourcePolicyRef"] != monitor.SourcePolicyRef {
			return fmt.Errorf("Initiative source monitor %s drifted from its Objective cadence", monitor.ID)
		}
	}
	return nil
}

func portableDigest(value any) string {
	payload, _ := json.Marshal(value)
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func cloneJSON[T any](value *T) *T {
	payload, _ := json.Marshal(value)
	var result T
	_ = json.Unmarshal(payload, &result)
	return &result
}
