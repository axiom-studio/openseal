package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

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
	activation              authoring.WorkforceActivationIntent
	agentDefinitions        []*agent.AgentDefinition
	agentDeployments        []*agent.AgentDeployment
	agentRunbooks           []workforceAgentRunbookApplication
	agentActivations        []workforce.DefinitionActivation
	skillBindings           []*capability.Binding
	teamDefinition          *team.Definition
	teamDeployment          *team.Deployment
	teamActivation          workforce.DefinitionActivation
	objectives              []workforceObjectiveApplication
	runbookActivations      []workforceRunbookActivationApplication
	project                 *Project
	projectExpectedRevision int64
	conversationEndpoints   []workforceConversationEndpointApplication
	callbackRegistrations   []workforceCallbackRegistrationApplication
	resources               []authoring.AppliedResourceReference
}

type workforceObjectiveApplication struct {
	value            *Objective
	expectedRevision int64
}

type workforceAgentRunbookApplication struct {
	definition   *agent.AgentDefinition
	deploymentID string
}

type workforceRunbookActivationApplication struct {
	value            *RunbookActivation
	expectedRevision int64
}

type workforceConversationEndpointApplication struct {
	value            *ExternalConversationEndpoint
	expectedRevision int64
}

type workforceCallbackRegistrationApplication struct {
	value            *CallbackRegistration
	expectedRevision int64
}

func prepareWorkforceCallbackRegistration(
	desired *CallbackRegistration,
	current *CallbackRegistration,
	actor authoring.ChangeSetActor,
	now time.Time,
) error {
	if desired == nil {
		return ErrInvalidCallbackRegistration
	}
	if current == nil {
		return desired.Validate()
	}
	if current.Scope != desired.Scope || current.Owner != desired.Owner || current.DeploymentID != desired.DeploymentID ||
		current.Status == CallbackRegistrationRetired || current.Revision+1 != desired.Revision {
		return authoring.ErrChangeSetRevision
	}
	desired.IngressRoute = current.IngressRoute
	desired.CreatedAt = current.CreatedAt
	desired.Lifecycle = cloneCallbackRegistration(current).Lifecycle
	desired.Lifecycle = append(desired.Lifecycle, CallbackRegistrationLifecycleEntry{
		Revision: desired.Revision, Action: callbackLifecycleAction(current.Status, &desired.Status),
		Actor: ActivityActor{Type: actor.Type, ID: actor.ID}, Reason: "apply reviewed workflow callback revision", At: now,
	})
	return desired.Validate()
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
		if err := resolveAgentApprovalDestinations(value, definition); err != nil {
			return nil, err
		}
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
		var continuation *workforce.ActivationContinuation
		if !activate {
			continuation = &workforce.ActivationContinuation{ChangeSetID: value.ID}
		}
		deployment := &agent.AgentDeployment{ID: deploymentID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, PreviousVersion: previous, RolloutStatus: rolloutStatus, Environment: value.Placement.Environment, Credentials: value.Placement.CredentialReferences[definition.ID], Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: definition.Authority.MaxConcurrentRuns}, Activation: continuation, Revision: revision, CreatedAt: now, UpdatedAt: now}
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
		application.agentRunbooks = append(application.agentRunbooks, workforceAgentRunbookApplication{definition: definition, deploymentID: deploymentID})
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
	var continuation *workforce.ActivationContinuation
	if !activate {
		continuation = &workforce.ActivationContinuation{ChangeSetID: value.ID}
	}
	application.teamDeployment = &team.Deployment{ID: value.Placement.TeamDeploymentID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Roster: roster, Status: teamStatus, Activation: continuation, Revision: revision, CreatedAt: now, UpdatedAt: now}
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

func resolveAgentApprovalDestinations(value *authoring.ChangeSet, definition *agent.AgentDefinition) error {
	if value == nil || definition == nil {
		return nil
	}
	blueprints := make(map[string]authoring.ConversationEndpointBlueprint, len(value.Result.Candidate.ConversationEndpoints))
	for _, endpoint := range value.Result.Candidate.ConversationEndpoints {
		blueprints[endpoint.ID] = endpoint
	}
	for index := range definition.Authority.ApprovalDestinations {
		blueprintID := strings.TrimSpace(definition.Authority.ApprovalDestinations[index].EndpointID)
		blueprint, ok := blueprints[blueprintID]
		placement, placed := value.Placement.ConversationEndpoints[blueprintID]
		if !ok || !placed || blueprint.Owner.Type != authoring.ConversationEndpointOwnerAgent || blueprint.Owner.ID != definition.ID || strings.TrimSpace(placement.ID) == "" {
			return fmt.Errorf("Agent %s approval destination %s has no reviewed owned conversation endpoint placement", definition.ID, blueprintID)
		}
		definition.Authority.ApprovalDestinations[index].EndpointID = strings.TrimSpace(placement.ID)
	}
	definition.Channels = nil
	for _, blueprint := range value.Result.Candidate.ConversationEndpoints {
		if blueprint.Owner.Type != authoring.ConversationEndpointOwnerAgent || blueprint.Owner.ID != definition.ID {
			continue
		}
		placement, placed := value.Placement.ConversationEndpoints[blueprint.ID]
		if !placed || strings.TrimSpace(placement.ID) == "" {
			return fmt.Errorf("Agent %s channel %s has no reviewed conversation endpoint placement", definition.ID, blueprint.ID)
		}
		purposes := make([]string, 0, len(blueprint.Purposes))
		for _, purpose := range blueprint.Purposes {
			purposes = append(purposes, string(purpose))
		}
		if len(purposes) == 0 {
			purposes = []string{string(authoring.ConversationEndpointPurposeConversation)}
		}
		definition.Channels = append(definition.Channels, agent.ChannelRoute{
			EndpointID: strings.TrimSpace(placement.ID), Trigger: strings.TrimSpace(blueprint.Handler.Trigger),
			MessageSelection: string(blueprint.Policy.MessageSelection), ReplyMode: string(blueprint.Policy.ReplyMode),
			IgnoreBots: blueprint.Policy.IgnoreBots, Purposes: purposes,
		})
	}
	return nil
}

func finishWorkforceApplication(value *authoring.ChangeSet, application *workforceApplication, deploymentByDefinition map[string]string) error {
	for _, source := range application.agentRunbooks {
		activations, err := materializeRunbookActivations(value, source.definition, source.deploymentID, application.objectives)
		if err != nil {
			return err
		}
		application.runbookActivations = append(application.runbookActivations, activations...)
	}
	for _, objective := range application.objectives {
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "objective", ID: objective.value.ID, Revision: objective.value.Revision})
	}
	for _, activation := range application.runbookActivations {
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "runbook", ID: activation.value.ID, Version: activation.value.DefinitionVersion, Revision: activation.value.Revision})
	}
	if value.Result.Candidate.Project != nil {
		project, err := materializeProject(value, application, deploymentByDefinition)
		if err != nil {
			return err
		}
		application.project = project
		application.projectExpectedRevision = value.Placement.ProjectExpectedRevision
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "project", ID: project.ID, Revision: project.Revision})
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
		var callbackAdapter *authoring.CallbackAdapterCapability
		if blueprint.CallbackAdapterID != "" {
			for index := range skillCapability.CallbackAdapters {
				if skillCapability.CallbackAdapters[index].ID == blueprint.CallbackAdapterID {
					callbackAdapter = &skillCapability.CallbackAdapters[index]
					break
				}
			}
			if callbackAdapter == nil || callbackAdapter.Provider != adapter.Provider ||
				!containsExactRuntimeString(callbackAdapter.EventTypes, capability.CallbackEventApprovalDecided) {
				return fmt.Errorf("conversation endpoint %s callback adapter is unavailable", blueprint.ID)
			}
			if err := enableWorkforceCallbackAdapter(value, blueprint, binding, callbackAdapter, activate); err != nil {
				return err
			}
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
		usesSlackMembership := blueprint.SkillID == "skill-slack" && blueprint.CallbackAdapterID == ""
		if address == "" && !usesSlackMembership {
			address = strings.TrimSpace(blueprint.Address)
		}
		if usesSlackMembership {
			address = ""
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
			Provider: adapter.Provider, Mode: blueprint.Mode,
			InstallationID: strings.TrimSpace(placement.InstallationID),
			ApplicationID:  strings.TrimSpace(placement.ApplicationID),
			Address:        address, Handler: handler,
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
		if callbackAdapter != nil {
			callbackStatus := CallbackRegistrationPaused
			if activate {
				callbackStatus = CallbackRegistrationActive
			}
			callbackID := strings.TrimSpace(placement.CallbackRegistrationID)
			if callbackID == "" {
				return fmt.Errorf("conversation endpoint %s callback has no reviewed placement", blueprint.ID)
			}
			callback := &CallbackRegistration{
				ID: callbackID, IngressRoute: uuid.NewString(), Scope: endpoint.Scope,
				Owner: endpoint.Owner, DeploymentID: endpoint.DeploymentID,
				Name: endpoint.Name + " interactions", Provider: endpoint.Provider,
				Adapter: CallbackAdapterReference{
					SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity,
					BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: callbackAdapter.ID,
				},
				Subscriptions: []CallbackSubscription{{
					EventType: capability.CallbackEventApprovalDecided, Consumer: "approvals", TargetID: endpoint.ID,
				}},
				Configuration: cloneMap(placement.CallbackConfiguration), Status: callbackStatus,
				Revision: placement.CallbackRegistrationExpectedRevision + 1, CreatedAt: now, UpdatedAt: now,
				Lifecycle: []CallbackRegistrationLifecycleEntry{{
					Revision: placement.CallbackRegistrationExpectedRevision + 1,
					Action:   CallbackRegistrationCreated,
					Actor:    ActivityActor{Type: value.ApplyReceipt.Actor.Type, ID: value.ApplyReceipt.Actor.ID},
					Reason:   "materialize reviewed workflow callback", At: now,
				}},
			}
			application.callbackRegistrations = append(application.callbackRegistrations, workforceCallbackRegistrationApplication{
				value: callback, expectedRevision: placement.CallbackRegistrationExpectedRevision,
			})
		}
	}
	return nil
}

func enableWorkforceCallbackAdapter(
	value *authoring.ChangeSet,
	blueprint authoring.ConversationEndpointBlueprint,
	binding *capability.Binding,
	adapter *authoring.CallbackAdapterCapability,
	activate bool,
) error {
	if !containsExactRuntimeString(binding.EnabledCallbackAdapters, adapter.ID) {
		binding.EnabledCallbackAdapters = append(binding.EnabledCallbackAdapters, adapter.ID)
		sort.Strings(binding.EnabledCallbackAdapters)
	}
	for _, credential := range adapter.Credentials {
		reference := value.Placement.CredentialReferences[blueprint.Owner.ID][credential.Name]
		if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
			if credential.Optional || !activate {
				continue
			}
			return fmt.Errorf("conversation endpoint %s callback requires opaque credential %s of kind %s", blueprint.ID, credential.Name, credential.Kind)
		}
		if reference.Kind != credential.Kind {
			return fmt.Errorf("conversation endpoint %s callback credential %s must use kind %s", blueprint.ID, credential.Name, credential.Kind)
		}
		binding.Credentials[credential.Name] = reference
	}
	return skill.ValidateBindingShape(binding)
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
			ID: workforceSkillBindingID(deploymentID, identity.ID, identity.Version), Scope: value.Scope, DeploymentID: deploymentID,
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

// workforceSkillBindingID preserves the single canonical binding identity for
// kernel-owned management capabilities. These capabilities are reconciled for
// every deployment by the embedding host; authoring may request their actions,
// but must update that same binding rather than materialize a parallel
// workforce binding with indistinguishable model actions.
func workforceSkillBindingID(deploymentID, skillID, version string) string {
	switch strings.TrimSpace(skillID) {
	case AgentManagementSkillID:
		return "bundled:agents"
	case ObjectiveManagementSkillID:
		return "bundled:objectives"
	case ProjectManagementSkillID:
		return "bundled:projects"
	case RunbookManagementSkillID:
		return "bundled:runbooks"
	case RunManagementSkillID:
		return "bundled:runs"
	case SkillManagementSkillID:
		return "bundled:skills@" + strings.TrimSpace(version)
	default:
		return "workforce:" + deploymentID + ":" + skillID
	}
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
		// The persisted authoring catalog is part of the reviewed ChangeSet and
		// already names the exact authorized Skill definition. Conversation-only
		// Skills do not need an Agent prompt/action requirement, so hosts may not
		// have emitted a separate placement entry for them. Preserve the catalog's
		// runtime ID and provenance instead of constructing a lossy unqualified
		// binding that the store would have to repair during apply.
		catalogSkill := value.Catalog.Skills[catalogID]
		runtimeID := strings.TrimSpace(catalogSkill.ID)
		if runtimeID == "" {
			runtimeID = catalogID
		}
		sourceIdentity := strings.TrimSpace(value.Placement.SkillSourceIdentities[agentID][catalogID])
		if sourceIdentity == "" {
			sourceIdentity = strings.TrimSpace(catalogSkill.SourceIdentity)
		}
		return capability.NewSkillIdentity(runtimeID, workforceSkillBindingVersion(value, agentID, catalogID, catalogVersion), sourceIdentity)
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

func synchronizeWorkforceRunbookResource(application *workforceApplication, activation *RunbookActivation) {
	if application == nil || activation == nil {
		return
	}
	for index := range application.resources {
		resource := &application.resources[index]
		if resource.Kind == "runbook" && resource.ID == activation.ID {
			resource.Version = activation.DefinitionVersion
			resource.Revision = activation.Revision
			return
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
	for index := range application.callbackRegistrations {
		registration := application.callbackRegistrations[index].value
		binding := bindings[registration.Adapter.BindingID]
		if binding == nil || binding.Revision < 1 || binding.SkillID != registration.Adapter.SkillID ||
			binding.SkillVersion != registration.Adapter.SkillVersion ||
			binding.SourceIdentity != registration.Adapter.SourceIdentity ||
			!containsExactRuntimeString(binding.EnabledCallbackAdapters, registration.Adapter.AdapterID) {
			return fmt.Errorf("callback registration %s lost its exact Skill adapter binding", registration.ID)
		}
		registration.Adapter.BindingRevision = binding.Revision
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
		key := authoring.WorkforceObjectiveKey(ownerType, definitionID, template.ID)
		placement := value.Placement.Objectives[key]
		revision := int64(1)
		if placement.ExpectedRevision > 0 {
			revision = placement.ExpectedRevision + 1
		}
		objective := &Objective{ID: placement.ID, Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Owner: ObjectiveOwner{Type: OwnerType(ownerType), ID: ownerID}, Title: template.Title, Goal: template.Goal, Status: status, Priority: template.Priority, Constraints: template.Constraints, SuccessCriteria: template.SuccessCriteria, Revision: revision, CreatedAt: value.ApplyReceipt.AppliedAt, UpdatedAt: value.ApplyReceipt.AppliedAt}
		if err := objective.Validate(); err != nil {
			return nil, fmt.Errorf("materialize Objective %s: %w", template.ID, err)
		}
		result = append(result, workforceObjectiveApplication{value: objective, expectedRevision: placement.ExpectedRevision})
	}
	return result, nil
}

func materializeRunbookActivations(value *authoring.ChangeSet, definition *agent.AgentDefinition, deploymentID string, objectives []workforceObjectiveApplication) ([]workforceRunbookActivationApplication, error) {
	if definition == nil || definition.Runbook == nil {
		return nil, nil
	}
	objectivesByID := make(map[string]*Objective, len(objectives))
	for _, objective := range objectives {
		if objective.value != nil {
			objectivesByID[objective.value.ID] = objective.value
		}
	}
	triggerIDs := make([]string, 0, len(definition.Runbook.Triggers))
	for triggerID := range definition.Runbook.Triggers {
		triggerIDs = append(triggerIDs, triggerID)
	}
	sort.Strings(triggerIDs)
	result := make([]workforceRunbookActivationApplication, 0, len(triggerIDs))
	for _, triggerID := range triggerIDs {
		trigger := definition.Runbook.Triggers[triggerID]
		placement, ok := value.Placement.Objectives[trigger.ObjectiveID]
		if !ok || strings.TrimSpace(placement.ID) == "" {
			return nil, fmt.Errorf("Runbook trigger %s Objective %s has no reviewed placement", triggerID, trigger.ObjectiveID)
		}
		objective := objectivesByID[placement.ID]
		if objective == nil {
			return nil, fmt.Errorf("Runbook trigger %s Objective %s was not materialized", triggerID, trigger.ObjectiveID)
		}
		input, err := materializeRunbookTriggerInput(trigger.Input)
		if err != nil {
			return nil, fmt.Errorf("Runbook trigger %s input: %w", triggerID, err)
		}
		policy := map[string]interface{}(nil)
		if blueprint := value.Result.Candidate.Project; blueprint != nil {
			for _, monitor := range blueprint.SourceMonitors {
				if monitor.ObjectiveRef != trigger.ObjectiveID || monitor.AssignedAgentDefinitionID != definition.ID {
					continue
				}
				if input == nil {
					input = map[string]interface{}{}
				}
				if input["sourceMonitorId"] != nil {
					return nil, fmt.Errorf("Runbook trigger %s is shared by multiple source monitors", triggerID)
				}
				input["projectId"], input["sourceMonitorId"] = value.Placement.ProjectID, monitor.ID
				policy = map[string]interface{}{"sourcePolicyRef": monitor.SourcePolicyRef}
			}
		}
		revision := value.Placement.AgentExpectedRevisions[definition.ID] + 1
		status := RunbookActivationActive
		if value.Result.Candidate.Activation == authoring.WorkforceActivationInactive {
			status = RunbookActivationPaused
		}
		activation := &RunbookActivation{
			ID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte(value.Scope.Kind+"\x00"+value.Scope.ID+"\x00"+deploymentID+"\x00"+definition.Runbook.ID+"\x00"+triggerID)).String(),
			Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Owner: objective.Owner, ObjectiveID: objective.ID,
			AssignedAgentID: deploymentID, DefinitionID: definition.Runbook.ID, DefinitionVersion: definition.Runbook.Version,
			TriggerID: triggerID, Trigger: trigger, Input: input, Policy: policy, Budget: runbookBudgetPolicyValue(trigger.Budget),
			MaximumConcurrent: trigger.MaximumConcurrent, Status: status, Revision: revision,
			CreatedAt: value.ApplyReceipt.AppliedAt, UpdatedAt: value.ApplyReceipt.AppliedAt,
		}
		activation.Trigger.ObjectiveID = objective.ID
		if err := activation.Validate(); err != nil {
			return nil, fmt.Errorf("materialize Runbook trigger %s: %w", triggerID, err)
		}
		result = append(result, workforceRunbookActivationApplication{value: activation, expectedRevision: value.Placement.AgentExpectedRevisions[definition.ID]})
	}
	return result, nil
}

func materializeRunbookTriggerInput(values map[string]runbook.Value) (map[string]interface{}, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]interface{}, len(values))
	for name, value := range values {
		if len(value.Literal) == 0 || value.Ref != "" || len(value.Template) > 0 {
			return nil, fmt.Errorf("%s must be a literal", name)
		}
		var decoded interface{}
		if err := json.Unmarshal(value.Literal, &decoded); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		result[name] = decoded
	}
	return result, nil
}

func materializeProject(value *authoring.ChangeSet, application *workforceApplication, deploymentByDefinition map[string]string) (*Project, error) {
	blueprint := value.Result.Candidate.Project
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
				return nil, fmt.Errorf("Project Objective reference %s has no placement", reference)
			}
		}
		return result, nil
	}
	ownerID := ""
	switch blueprint.Owner.Type {
	case authoring.ProjectOwnerAgent:
		ownerID = deploymentByDefinition[blueprint.Owner.DefinitionID]
	case authoring.ProjectOwnerTeam:
		if application.teamDeployment != nil && application.teamDefinition.ID == blueprint.Owner.DefinitionID {
			ownerID = application.teamDeployment.ID
		}
	}
	if ownerID == "" {
		return nil, fmt.Errorf("Project owner has no deployed placement")
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
	status := ProjectStatusActive
	if activation == authoring.WorkforceActivationInactive {
		status = ProjectStatusDraft
	}
	project := &Project{
		ID: value.Placement.ProjectID, Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Title: blueprint.Title, Purpose: blueprint.Purpose,
		Status: status, Owner: ObjectiveOwner{Type: OwnerType(blueprint.Owner.Type), ID: ownerID}, ObjectiveRefs: objectiveRefs,
		Policy: cloneMap(blueprint.Policy), Revision: value.Placement.ProjectExpectedRevision + 1, CreatedAt: now, UpdatedAt: now,
	}
	for index, definition := range application.agentDefinitions {
		deployment := application.agentDeployments[index]
		project.AgentRefs = append(project.AgentRefs,
			ResourceReference{Kind: ResourceKindAgentDefinition, ID: definition.ID, Version: definition.Version},
			ResourceReference{Kind: ResourceKindAgentDeployment, ID: deployment.ID, Version: definition.Version, Revision: deployment.Revision},
		)
	}
	if application.teamDefinition != nil {
		project.TeamRefs = append(project.TeamRefs,
			ResourceReference{Kind: ResourceKindTeamDefinition, ID: application.teamDefinition.ID, Version: application.teamDefinition.Version},
			ResourceReference{Kind: ResourceKindTeamDeployment, ID: application.teamDeployment.ID, Version: application.teamDefinition.Version, Revision: application.teamDeployment.Revision},
		)
	}
	for _, source := range blueprint.Milestones {
		refs, err := translate(source.ObjectiveRefs)
		if err != nil {
			return nil, err
		}
		project.Milestones = append(project.Milestones, ProjectMilestone{ID: source.ID, Title: source.Title, Status: MilestonePending, ObjectiveRefs: refs})
	}
	for _, source := range blueprint.Hypotheses {
		project.Hypotheses = append(project.Hypotheses, ProjectHypothesis{ID: source.ID, Statement: source.Statement, Confidence: source.Confidence, Status: HypothesisOpen, UpdatedAt: now})
	}
	for _, source := range blueprint.SourceMonitors {
		objectiveID := objectiveIDs[source.ObjectiveRef]
		agentID := deploymentByDefinition[source.AssignedAgentDefinitionID]
		if objectiveID == "" || agentID == "" {
			return nil, fmt.Errorf("Project source monitor %s has unresolved Objective or Agent placement", source.ID)
		}
		project.SourceMonitors = append(project.SourceMonitors, SourceMonitorReference{
			ID: source.ID, ObjectiveID: objectiveID, AssignedAgentID: agentID, SkillID: source.SkillID, SkillVersion: source.SkillVersion,
			Action: source.Action, SourcePolicyRef: source.SourcePolicyRef, Deduplication: SourceMonitorDeduplication(source.Deduplication),
		})
	}
	for _, source := range blueprint.Deliverables {
		refs, err := translate(source.ObjectiveRefs)
		if err != nil {
			return nil, err
		}
		project.Deliverables = append(project.Deliverables, ProjectDeliverable{ID: source.ID, Title: source.Title, Status: DeliverablePlanned, ObjectiveRefs: refs})
	}
	if err := project.Validate(); err != nil {
		return nil, fmt.Errorf("materialize Project: %w", err)
	}
	if err := validateMaterializedProjectMonitors(project, application); err != nil {
		return nil, err
	}
	idempotency := sha256.Sum256([]byte("workforce-change-set\x00" + value.ID + "\x00" + value.ApplyReceipt.IdempotencyKey))
	project.IdempotencyKeyHash = hex.EncodeToString(idempotency[:])
	fingerprint, err := projectCreationFingerprint(project)
	if err != nil {
		return nil, fmt.Errorf("fingerprint Project: %w", err)
	}
	project.CreationFingerprint = fingerprint
	return project, nil
}

func validateMaterializedProjectMonitors(project *Project, application *workforceApplication) error {
	byID := make(map[string]*Objective, len(application.objectives))
	for _, objective := range application.objectives {
		byID[objective.value.ID] = objective.value
	}
	definitions := make(map[string]*agent.AgentDefinition, len(application.agentDefinitions))
	for _, definition := range application.agentDefinitions {
		if definition != nil && definition.Runbook != nil {
			definitions[definition.Runbook.ID+"\x00"+definition.Runbook.Version] = definition
		}
	}
	for _, monitor := range project.SourceMonitors {
		objective := byID[monitor.ObjectiveID]
		if objective == nil || objective.Owner != project.Owner {
			return fmt.Errorf("Project source monitor %s has no matching owned Objective", monitor.ID)
		}
		matched := false
		for _, activation := range application.runbookActivations {
			item := activation.value
			if item.ObjectiveID != monitor.ObjectiveID || item.AssignedAgentID != monitor.AssignedAgentID || item.Input["projectId"] != project.ID || item.Input["sourceMonitorId"] != monitor.ID || item.Policy["sourcePolicyRef"] != monitor.SourcePolicyRef {
				continue
			}
			definition := definitions[item.DefinitionID+"\x00"+item.DefinitionVersion]
			if definition == nil || definition.Runbook == nil {
				continue
			}
			for _, step := range definition.Runbook.Steps {
				if step.Kind == runbook.StepAction && step.Action != nil && step.Action.SkillID == monitor.SkillID && step.Action.SkillVersion == monitor.SkillVersion && step.Action.Action == monitor.Action {
					matched = true
					break
				}
			}
		}
		if !matched {
			return fmt.Errorf("Project source monitor %s has no matching Objective-owned Runbook action", monitor.ID)
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
