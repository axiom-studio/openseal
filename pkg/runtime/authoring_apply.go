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
	agentDefinitions []*agent.AgentDefinition
	agentDeployments []*agent.AgentDeployment
	agentActivations []workforce.DefinitionActivation
	skillBindings    []*capability.Binding
	teamDefinition   *team.Definition
	teamDeployment   *team.Deployment
	teamActivation   workforce.DefinitionActivation
	objectives       []workforceObjectiveApplication
	resources        []authoring.AppliedResourceReference
}

type workforceObjectiveApplication struct {
	value            *Objective
	expectedRevision int64
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
		revision := int64(1)
		previous := ""
		if value.Mode == authoring.ModeAmend {
			revision = value.Placement.AgentExpectedRevisions[definition.ID] + 1
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
		application.objectives = append(application.objectives, materializeObjectives(value, "agent", definition.ID, deployment.ID, definition.ObjectiveTemplates)...)
	}
	if value.Result.Candidate.Team == nil {
		for _, objective := range application.objectives {
			application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "objective", ID: objective.value.ID, Revision: objective.value.Revision})
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
	revision := int64(1)
	if value.Mode == authoring.ModeAmend {
		revision = value.Placement.TeamExpectedRevision + 1
	}
	application.teamDeployment = &team.Deployment{ID: value.Placement.TeamDeploymentID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Roster: roster, Status: team.DeploymentActive, Revision: revision, CreatedAt: now, UpdatedAt: now}
	if err := application.teamDeployment.Validate(definition); err != nil {
		return nil, err
	}
	application.teamActivation = workforce.DefinitionActivation{ID: value.ApplyReceipt.ID + ":team", Scope: scope, DeploymentID: application.teamDeployment.ID, DefinitionID: definition.ID, ToVersion: definition.Version, DeploymentRevision: revision, Reason: "workforce_change_set:" + value.ID, ActorType: value.ApplyReceipt.Actor.Type, ActorID: value.ApplyReceipt.Actor.ID, CreatedAt: now}
	application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "team_definition", ID: definition.ID, Version: definition.Version}, authoring.AppliedResourceReference{Kind: "team_deployment", ID: application.teamDeployment.ID, Version: definition.Version, Revision: revision})
	application.objectives = append(application.objectives, materializeObjectives(value, "team", definition.ID, application.teamDeployment.ID, definition.ObjectiveTemplates)...)
	for _, objective := range application.objectives {
		application.resources = append(application.resources, authoring.AppliedResourceReference{Kind: "objective", ID: objective.value.ID, Revision: objective.value.Revision})
	}
	sort.Slice(application.resources, func(i, j int) bool {
		return application.resources[i].Kind+application.resources[i].ID < application.resources[j].Kind+application.resources[j].ID
	})
	return application, nil
}

func materializeWorkforceSkillBindings(value *authoring.ChangeSet, definition *agent.AgentDefinition, deploymentID string) ([]*capability.Binding, error) {
	bindings := make([]*capability.Binding, 0, len(definition.SkillRequirements))
	for _, requirement := range definition.SkillRequirements {
		skillCapability, ok := value.Generation.Request.Catalog.Skills[requirement.SkillID]
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
		if workforceRiskRank(definition.Authority.MaximumRisk) < workforceRiskRank(maximumRisk) {
			maximumRisk = definition.Authority.MaximumRisk
		}
		bindings = append(bindings, &capability.Binding{
			ID: "workforce:" + deploymentID + ":" + requirement.SkillID, Scope: value.Scope, DeploymentID: deploymentID,
			SkillID: requirement.SkillID, SkillVersion: skillCapability.Version, AllowedActions: allowed,
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

func materializeObjectives(value *authoring.ChangeSet, ownerType, definitionID, ownerID string, templates []workforce.ObjectiveTemplate) []workforceObjectiveApplication {
	result := make([]workforceObjectiveApplication, 0, len(templates))
	for _, template := range templates {
		var cadence *ObjectiveCadence
		if len(template.Cadence) > 0 {
			payload, _ := json.Marshal(template.Cadence)
			var decoded ObjectiveCadence
			if json.Unmarshal(payload, &decoded) == nil {
				cadence = &decoded
			}
		}
		key := authoring.WorkforceObjectiveKey(ownerType, definitionID, template.ID)
		placement := value.Placement.Objectives[key]
		revision := int64(1)
		if placement.ExpectedRevision > 0 {
			revision = placement.ExpectedRevision + 1
		}
		result = append(result, workforceObjectiveApplication{value: &Objective{ID: placement.ID, Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, Owner: ObjectiveOwner{Type: OwnerType(ownerType), ID: ownerID}, Title: template.Title, Goal: template.Goal, Status: ObjectiveStatusActive, Priority: template.Priority, Cadence: cadence, EventRules: template.EventRules, Constraints: template.Constraints, SuccessCriteria: template.SuccessCriteria, Revision: revision, CreatedAt: value.ApplyReceipt.AppliedAt, UpdatedAt: value.ApplyReceipt.AppliedAt}, expectedRevision: placement.ExpectedRevision})
	}
	return result
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
