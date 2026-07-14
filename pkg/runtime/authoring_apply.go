package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type workforceApplication struct {
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
	resources                  []authoring.AppliedResourceReference
}

type workforceObjectiveApplication struct {
	value            *Objective
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
	return deployments
}

func materializeWorkforceApplication(value *authoring.ChangeSet) (*workforceApplication, error) {
	if value == nil || value.ApplyReceipt == nil {
		return nil, fmt.Errorf("applied workforce aggregate is incomplete")
	}
	now, scope := value.ApplyReceipt.AppliedAt, value.Scope
	application := &workforceApplication{}
	deploymentByDefinition := map[string]string{}
	for index, source := range value.Result.Candidate.Agents {
		if source == nil {
			return nil, fmt.Errorf("Agent definition is required")
		}
		definition := cloneJSON(source)
		definition.CreatedAt = now
		definition.Digest = ""
		definition.Digest = portableDigest(definition)
		deploymentID := value.Placement.AgentDeploymentIDs[definition.ID]
		deploymentByDefinition[definition.ID] = deploymentID
		expectedRevision := value.Placement.AgentExpectedRevisions[definition.ID]
		revision := expectedRevision + 1
		previous := ""
		if expectedRevision > 0 {
			previous = "__load__"
		}
		deployment := &agent.AgentDeployment{ID: deploymentID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, PreviousVersion: previous, RolloutStatus: agent.RolloutActive, Environment: value.Placement.Environment, Credentials: value.Placement.CredentialReferences[definition.ID], Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: definition.Authority.MaxConcurrentRuns}, Revision: revision, CreatedAt: now, UpdatedAt: now}
		if err := deployment.Validate(); err != nil {
			return nil, err
		}
		activation := workforce.DefinitionActivation{ID: value.ApplyReceipt.ID + fmt.Sprintf(":agent:%d", index), Scope: scope, DeploymentID: deployment.ID, DefinitionID: definition.ID, ToVersion: definition.Version, DeploymentRevision: revision, Reason: "workforce_change_set:" + value.ID, ActorType: value.ApplyReceipt.Actor.Type, ActorID: value.ApplyReceipt.Actor.ID, CreatedAt: now}
		application.agentDefinitions = append(application.agentDefinitions, definition)
		application.agentDeployments = append(application.agentDeployments, deployment)
		application.agentActivations = append(application.agentActivations, activation)
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "agent_definition", ID: definition.ID, Version: definition.Version}, authoring.AppliedResourceReference{Kind: "agent_deployment", ID: deployment.ID, Version: definition.Version, Revision: revision})
		bindings, err := materializeWorkforceSkillBindings(value, definition, deployment.ID)
		if err != nil {
			return nil, err
		}
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
	definition.CreatedAt = now
	definition.Digest = ""
	definition.Digest = portableDigest(definition)
	application.teamDefinition = definition
	roster := make([]team.RosterAssignment, 0, len(value.Result.Candidate.Assignments))
	for _, assignment := range value.Result.Candidate.Assignments {
		roster = append(roster, team.RosterAssignment{ID: assignment.ID, RoleID: assignment.RoleID, AgentDeploymentID: deploymentByDefinition[assignment.AgentDefinitionID], DisplayName: assignment.DisplayName})
	}
	revision := value.Placement.TeamExpectedRevision + 1
	application.teamDeployment = &team.Deployment{ID: value.Placement.TeamDeploymentID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Roster: roster, Status: team.DeploymentActive, Revision: revision, CreatedAt: now, UpdatedAt: now}
	if err := application.teamDeployment.Validate(definition); err != nil {
		return nil, err
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
	sort.Slice(application.resources, func(i, j int) bool {
		return application.resources[i].Kind+application.resources[i].ID < application.resources[j].Kind+application.resources[j].ID
	})
	return nil
}

func materializeWorkforceSkillBindings(value *authoring.ChangeSet, definition *agent.AgentDefinition, deploymentID string) ([]*capability.Binding, error) {
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
			reference := value.Placement.CredentialReferences[definition.ID][credential.Kind]
			if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
				if credential.Optional {
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
		bindings = append(bindings, &capability.Binding{
			ID: "workforce:" + deploymentID + ":" + requirement.SkillID, Scope: value.Scope, DeploymentID: deploymentID,
			SkillID: requirement.SkillID, SkillVersion: skillCapability.Version,
			SourceIdentity: strings.TrimSpace(value.Placement.SkillSourceIdentities[definition.ID][requirement.SkillID]), AllowedActions: allowed,
			EnablePrompt: requirement.PromptRequired, MaximumRisk: maximumRisk, Credentials: credentials, Revision: 1,
		})
	}
	return bindings, nil
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
	if binding == nil || definition == nil || definition.ID != binding.SkillID || definition.Version != binding.SkillVersion {
		return fmt.Errorf("Skill binding %s does not match an installed immutable Skill definition", binding.ID)
	}
	definitionSourceIdentity := ""
	if definition.Source != nil {
		definitionSourceIdentity = strings.TrimSpace(definition.Source.Identity)
	}
	if strings.TrimSpace(binding.SourceIdentity) != definitionSourceIdentity {
		return fmt.Errorf("Skill binding %s does not match the selected immutable Skill source", binding.ID)
	}
	if binding.EnablePrompt && definition.Prompt == nil {
		return fmt.Errorf("Skill binding %s requires a prompt the Skill does not define", binding.ID)
	}
	for _, actionName := range binding.AllowedActions {
		action, ok := definition.Actions[actionName]
		if !ok || workforceRiskRank(action.Risk) > workforceRiskRank(binding.MaximumRisk) {
			return fmt.Errorf("Skill binding %s cannot authorize action %s", binding.ID, actionName)
		}
		for _, requirement := range action.Credentials {
			reference, exists := binding.Credentials[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || reference.Kind != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("Skill binding %s is missing credential %s", binding.ID, requirement.Name)
			}
		}
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

func materializeObjectives(value *authoring.ChangeSet, ownerType, definitionID, ownerID string, templates []workforce.ObjectiveTemplate, deploymentByDefinition map[string]string) ([]workforceObjectiveApplication, error) {
	result := make([]workforceObjectiveApplication, 0, len(templates))
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
		key := authoring.WorkforceObjectiveKey(ownerType, definitionID, template.ID)
		placement := value.Placement.Objectives[key]
		revision := int64(1)
		if placement.ExpectedRevision > 0 {
			revision = placement.ExpectedRevision + 1
		}
		objective := &Objective{ID: placement.ID, Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Owner: ObjectiveOwner{Type: OwnerType(ownerType), ID: ownerID}, Title: template.Title, Goal: template.Goal, Status: ObjectiveStatusActive, Priority: template.Priority, Cadence: cadence, EventRules: template.EventRules, Constraints: template.Constraints, SuccessCriteria: template.SuccessCriteria, Revision: revision, CreatedAt: value.ApplyReceipt.AppliedAt, UpdatedAt: value.ApplyReceipt.AppliedAt}
		if err := objective.Validate(); err != nil {
			return nil, fmt.Errorf("materialize Objective %s: %w", template.ID, err)
		}
		result = append(result, workforceObjectiveApplication{value: objective, expectedRevision: placement.ExpectedRevision})
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
	initiative := &Initiative{
		ID: value.Placement.InitiativeID, Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Title: blueprint.Title, Purpose: blueprint.Purpose,
		Status: InitiativeStatusActive, Owner: ObjectiveOwner{Type: OwnerType(blueprint.Owner.Type), ID: ownerID}, ObjectiveRefs: objectiveRefs,
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
