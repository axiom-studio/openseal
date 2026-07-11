package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

var (
	ErrDefinitionNotFound = errors.New("team definition not found")
	ErrDeploymentNotFound = errors.New("team deployment not found")
	ErrRevisionConflict   = errors.New("team deployment revision conflict")
)

type AgentResolver interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	GetDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
}

type Registry struct {
	store  Store
	agents AgentResolver
	now    func() time.Time
	newID  func() string
}

func NewRegistry(agents AgentResolver) *Registry {
	return NewRegistryWithStore(NewMemoryStore(), agents)
}

func NewRegistryWithStore(store Store, agents AgentResolver) *Registry {
	return &Registry{store: store, agents: agents, now: time.Now, newID: uuid.NewString}
}

func (r *Registry) RegisterDefinition(ctx context.Context, definition *Definition) (*Definition, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("team registry is not configured")
	}
	candidate, err := prepareDefinition(definition, r.now().UTC())
	if err != nil {
		return nil, err
	}
	if err := r.store.CreateTeamDefinition(ctx, candidate); err != nil {
		return nil, err
	}
	return cloneDefinition(candidate), nil
}

func (r *Registry) GetDefinition(ctx context.Context, id, version string) (*Definition, error) {
	return r.store.GetTeamDefinition(ctx, id, version)
}

func (r *Registry) ListDefinitionVersions(ctx context.Context, id string) ([]*Definition, error) {
	return r.store.ListTeamDefinitionVersions(ctx, id)
}

func (r *Registry) CreateDeployment(ctx context.Context, deployment *Deployment, actorType, actorID, reason string) (*Deployment, *workforce.DefinitionActivation, error) {
	if r == nil || r.store == nil || r.agents == nil {
		return nil, nil, errors.New("team registry and Agent resolver are required")
	}
	candidate := cloneDeployment(deployment)
	if candidate == nil {
		return nil, nil, errors.New("team deployment is required")
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = r.now().UTC()
	}
	candidate.UpdatedAt = candidate.CreatedAt
	if candidate.Revision == 0 {
		candidate.Revision = 1
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("team deployment activation actor is required")
	}
	definition, err := r.store.GetTeamDefinition(ctx, candidate.DefinitionID, candidate.ActiveVersion)
	if err != nil {
		return nil, nil, err
	}
	if err := candidate.Validate(definition); err != nil {
		return nil, nil, err
	}
	if err := r.validateRoster(ctx, definition, candidate); err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, candidate); err != nil {
		return nil, nil, err
	}
	activation := workforce.DefinitionActivation{
		ID: r.newID(), Scope: candidate.Scope, DeploymentID: candidate.ID, DefinitionID: candidate.DefinitionID,
		ToVersion: candidate.ActiveVersion, DeploymentRevision: candidate.Revision, Reason: strings.TrimSpace(reason),
		ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: candidate.CreatedAt,
	}
	if err := r.store.CreateTeamDeployment(ctx, candidate, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(candidate), &copyActivation, nil
}

func (r *Registry) GetDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*Deployment, error) {
	return r.store.GetTeamDeployment(ctx, scope, id)
}

func (r *Registry) ActivateDefinition(ctx context.Context, scope capability.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*Deployment, *workforce.DefinitionActivation, error) {
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("team deployment activation actor is required")
	}
	if strings.EqualFold(strings.TrimSpace(actorType), "agent") {
		return nil, nil, errors.New("agents must activate Team behavior changes through the amendment workflow")
	}
	current, err := r.store.GetTeamDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	definition, err := r.store.GetTeamDefinition(ctx, current.DefinitionID, version)
	if err != nil {
		return nil, nil, err
	}
	updated := cloneDeployment(current)
	updated.ActiveVersion = version
	updated.Status = DeploymentActive
	updated.Revision++
	updated.UpdatedAt = r.now().UTC()
	if err := updated.Validate(definition); err != nil {
		return nil, nil, err
	}
	if err := r.validateRoster(ctx, definition, updated); err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, updated); err != nil {
		return nil, nil, err
	}
	activation := workforce.DefinitionActivation{
		ID: r.newID(), Scope: scope, DeploymentID: deploymentID, DefinitionID: updated.DefinitionID,
		FromVersion: current.ActiveVersion, ToVersion: version, DeploymentRevision: updated.Revision,
		Reason: strings.TrimSpace(reason), ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: updated.UpdatedAt,
	}
	if err := r.store.UpdateTeamDeployment(ctx, updated, expectedRevision, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(updated), &copyActivation, nil
}

func (r *Registry) ListActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	return r.store.ListTeamDefinitionActivations(ctx, scope, deploymentID)
}

func (r *Registry) validateRoster(ctx context.Context, definition *Definition, deployment *Deployment) error {
	roles := make(map[string]RoleSlot, len(definition.Roles))
	for _, role := range definition.Roles {
		roles[role.ID] = role
	}
	for _, assignment := range deployment.Roster {
		agentDeployment, err := r.agents.GetDeployment(ctx, deployment.Scope, assignment.AgentDeploymentID)
		if err != nil {
			return errors.New("team roster Agent deployment is unavailable in scope")
		}
		agentDefinition, err := r.agents.GetDefinition(ctx, agentDeployment.DefinitionID, agentDeployment.ActiveVersion)
		if err != nil {
			return errors.New("team roster Agent definition is unavailable")
		}
		role := roles[assignment.RoleID]
		if len(role.RequiredDefinitionIDs) > 0 && !contains(role.RequiredDefinitionIDs, agentDefinition.ID) {
			return errors.New("team roster Agent definition does not satisfy its role")
		}
		availableSkills := make(map[string]bool, len(agentDefinition.SkillRequirements))
		for _, requirement := range agentDefinition.SkillRequirements {
			availableSkills[requirement.SkillID] = true
		}
		for _, skillID := range role.RequiredSkillIDs {
			if !availableSkills[skillID] {
				return errors.New("team roster Agent skills do not satisfy its role")
			}
		}
	}
	return nil
}

func prepareDefinition(definition *Definition, now time.Time) (*Definition, error) {
	candidate := cloneDefinition(definition)
	if candidate == nil {
		return nil, errors.New("team definition is required")
	}
	candidate.ID = strings.TrimSpace(candidate.ID)
	candidate.Version = strings.TrimSpace(candidate.Version)
	candidate.DisplayName = strings.TrimSpace(candidate.DisplayName)
	candidate.Purpose = strings.TrimSpace(candidate.Purpose)
	candidate.Digest = ""
	candidate.CreatedAt = time.Time{}
	for index := range candidate.Roles {
		candidate.Roles[index].ID = strings.TrimSpace(candidate.Roles[index].ID)
		candidate.Roles[index].RequiredSkillIDs = normalizedStrings(candidate.Roles[index].RequiredSkillIDs)
		candidate.Roles[index].RequiredDefinitionIDs = normalizedStrings(candidate.Roles[index].RequiredDefinitionIDs)
	}
	candidate.OperatingPrinciples = normalizedStrings(candidate.OperatingPrinciples)
	candidate.Approvals.ApproverRoleIDs = normalizedStrings(candidate.Approvals.ApproverRoleIDs)
	candidate.Approvals.ApproverPrincipals = normalizedStrings(candidate.Approvals.ApproverPrincipals)
	candidate.Amendments.AllowedFields = normalizedStrings(candidate.Amendments.AllowedFields)
	candidate.Amendments.ApproverPrincipals = normalizedStrings(candidate.Amendments.ApproverPrincipals)
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	candidate.Digest = "sha256:" + hex.EncodeToString(digest[:])
	candidate.CreatedAt = now
	return candidate, nil
}

func validateNarrowing(definition *Definition, deployment *Deployment) error {
	if deployment.Restrictions.MaximumRisk != "" && riskRank(deployment.Restrictions.MaximumRisk) > riskRank(definition.Approvals.MaximumRisk) {
		return errors.New("team deployment risk cannot widen definition authority")
	}
	if deployment.Restrictions.MaximumConcurrency > 0 && definition.Delegation.MaximumConcurrent > 0 &&
		deployment.Restrictions.MaximumConcurrency > definition.Delegation.MaximumConcurrent {
		return errors.New("team deployment concurrency cannot widen definition authority")
	}
	return nil
}

func riskRank(risk capability.RiskLevel) int {
	switch risk {
	case capability.RiskLevelRead:
		return 0
	case capability.RiskLevelWrite:
		return 1
	case capability.RiskLevelExternal:
		return 2
	case capability.RiskLevelProduction:
		return 3
	case capability.RiskLevelDestructive:
		return 4
	default:
		return -1
	}
}

func normalizedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
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

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func definitionKey(id, version string) string { return id + "\x00" + version }

func deploymentKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}

func cloneDefinition(value *Definition) *Definition {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result Definition
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneDeployment(value *Deployment) *Deployment {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result Deployment
	_ = json.Unmarshal(encoded, &result)
	return &result
}
