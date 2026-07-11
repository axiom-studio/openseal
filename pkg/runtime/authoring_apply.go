package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type workforceApplication struct {
	agentDefinitions []*agent.AgentDefinition
	agentDeployments []*agent.AgentDeployment
	agentActivations []workforce.DefinitionActivation
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
