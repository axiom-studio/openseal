package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

var (
	ErrDefinitionNotFound = errors.New("agent definition not found")
	ErrDeploymentNotFound = errors.New("agent deployment not found")
	ErrRevisionConflict   = errors.New("agent deployment revision conflict")
)

type Registry struct {
	mu          sync.RWMutex
	definitions map[string]*AgentDefinition
	deployments map[string]*AgentDeployment
	activations map[string][]DefinitionActivation
	now         func() time.Time
	newID       func() string
}

func NewRegistry() *Registry {
	return &Registry{
		definitions: make(map[string]*AgentDefinition), deployments: make(map[string]*AgentDeployment),
		activations: make(map[string][]DefinitionActivation), now: time.Now, newID: uuid.NewString,
	}
}

func (r *Registry) RegisterDefinition(_ context.Context, definition *AgentDefinition) (*AgentDefinition, error) {
	if r == nil {
		return nil, errors.New("agent registry is not configured")
	}
	candidate := cloneDefinition(definition)
	canonicalizeDefinition(candidate)
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = r.now().UTC()
	}
	providedDigest := candidate.Digest
	candidate.Digest = ""
	digest := definitionDigest(candidate)
	if providedDigest != "" && providedDigest != digest {
		return nil, errors.New("agent definition digest does not match content")
	}
	candidate.Digest = digest
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	key := definitionKey(candidate.ID, candidate.Version)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.definitions[key] != nil {
		return nil, errors.New("agent definition versions are immutable")
	}
	r.definitions[key] = cloneDefinition(candidate)
	return cloneDefinition(candidate), nil
}

func (r *Registry) GetDefinition(_ context.Context, id, version string) (*AgentDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value := r.definitions[definitionKey(id, version)]
	if value == nil {
		return nil, ErrDefinitionNotFound
	}
	return cloneDefinition(value), nil
}

func (r *Registry) ListDefinitionVersions(_ context.Context, id string) ([]*AgentDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*AgentDefinition, 0)
	for _, definition := range r.definitions {
		if definition.ID == id {
			result = append(result, cloneDefinition(definition))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	return result, nil
}

func (r *Registry) CreateDeployment(_ context.Context, deployment *AgentDeployment, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	if r == nil {
		return nil, nil, errors.New("agent registry is not configured")
	}
	candidate := cloneDeployment(deployment)
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = r.now().UTC()
	}
	candidate.UpdatedAt = candidate.CreatedAt
	if candidate.Revision == 0 {
		candidate.Revision = 1
	}
	candidate.SkillBindingIDs = normalizedStrings(candidate.SkillBindingIDs)
	if err := candidate.Validate(); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("deployment activation actor is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	definition := r.definitions[definitionKey(candidate.DefinitionID, candidate.ActiveVersion)]
	if definition == nil {
		return nil, nil, ErrDefinitionNotFound
	}
	if err := validateNarrowing(definition, candidate); err != nil {
		return nil, nil, err
	}
	key := deploymentKey(candidate.Scope, candidate.ID)
	if r.deployments[key] != nil {
		return nil, nil, errors.New("agent deployment already exists")
	}
	activation := DefinitionActivation{
		ID: r.newID(), Scope: candidate.Scope, DeploymentID: candidate.ID, DefinitionID: candidate.DefinitionID,
		ToVersion: candidate.ActiveVersion, DeploymentRevision: candidate.Revision, Reason: strings.TrimSpace(reason),
		ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: candidate.CreatedAt,
	}
	r.deployments[key] = cloneDeployment(candidate)
	r.activations[key] = append(r.activations[key], activation)
	copyActivation := activation
	return cloneDeployment(candidate), &copyActivation, nil
}

func (r *Registry) GetDeployment(_ context.Context, scope capability.ScopeReference, id string) (*AgentDeployment, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value := r.deployments[deploymentKey(scope, id)]
	if value == nil {
		return nil, ErrDeploymentNotFound
	}
	return cloneDeployment(value), nil
}

func (r *Registry) ActivateDefinition(_ context.Context, scope capability.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("deployment activation actor is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := deploymentKey(scope, deploymentID)
	current := r.deployments[key]
	if current == nil {
		return nil, nil, ErrDeploymentNotFound
	}
	if current.Revision != expectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	if current.ActiveVersion == version {
		return nil, nil, errors.New("agent deployment already uses the requested definition version")
	}
	definition := r.definitions[definitionKey(current.DefinitionID, version)]
	if definition == nil {
		return nil, nil, ErrDefinitionNotFound
	}
	updated := cloneDeployment(current)
	updated.PreviousVersion = current.ActiveVersion
	updated.ActiveVersion = version
	updated.RolloutStatus = RolloutActive
	updated.Revision++
	updated.UpdatedAt = r.now().UTC()
	if err := validateNarrowing(definition, updated); err != nil {
		return nil, nil, err
	}
	activation := DefinitionActivation{
		ID: r.newID(), Scope: scope, DeploymentID: deploymentID, DefinitionID: updated.DefinitionID,
		FromVersion: current.ActiveVersion, ToVersion: version, DeploymentRevision: updated.Revision,
		Reason: strings.TrimSpace(reason), ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: updated.UpdatedAt,
	}
	r.deployments[key] = cloneDeployment(updated)
	r.activations[key] = append(r.activations[key], activation)
	copyActivation := activation
	return cloneDeployment(updated), &copyActivation, nil
}

func (r *Registry) RollbackDefinition(ctx context.Context, scope capability.ScopeReference, deploymentID string, expectedRevision int64, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	current, err := r.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(current.PreviousVersion) == "" {
		return nil, nil, errors.New("agent deployment has no previous definition version")
	}
	return r.ActivateDefinition(ctx, scope, deploymentID, current.PreviousVersion, expectedRevision, actorType, actorID, reason)
}

func (r *Registry) ListActivations(_ context.Context, scope capability.ScopeReference, deploymentID string) ([]DefinitionActivation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	values := r.activations[deploymentKey(scope, deploymentID)]
	result := make([]DefinitionActivation, len(values))
	copy(result, values)
	return result, nil
}

func canonicalizeDefinition(value *AgentDefinition) {
	if value == nil {
		return
	}
	value.ID = strings.TrimSpace(value.ID)
	value.Version = strings.TrimSpace(value.Version)
	value.OperatingPrinciples = normalizedStrings(value.OperatingPrinciples)
	value.Authority.AllowedSkillIDs = normalizedStrings(value.Authority.AllowedSkillIDs)
	value.Amendments.AllowedFields = normalizedStrings(value.Amendments.AllowedFields)
	for index := range value.SkillRequirements {
		value.SkillRequirements[index].RequiredActions = normalizedStrings(value.SkillRequirements[index].RequiredActions)
	}
	sort.Slice(value.SkillRequirements, func(i, j int) bool { return value.SkillRequirements[i].SkillID < value.SkillRequirements[j].SkillID })
	sort.Slice(value.ObjectiveTemplates, func(i, j int) bool { return value.ObjectiveTemplates[i].ID < value.ObjectiveTemplates[j].ID })
	sort.Slice(value.Evaluations, func(i, j int) bool { return value.Evaluations[i].ID < value.Evaluations[j].ID })
}

func definitionDigest(value *AgentDefinition) string {
	copy := cloneDefinition(value)
	copy.Digest = ""
	copy.CreatedAt = time.Time{}
	encoded, _ := json.Marshal(copy)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func definitionKey(id, version string) string {
	return strings.TrimSpace(id) + "@" + strings.TrimSpace(version)
}
func deploymentKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + ":" + scope.ID + ":" + strings.TrimSpace(id)
}

func cloneDefinition(value *AgentDefinition) *AgentDefinition {
	if value == nil {
		return nil
	}
	var result AgentDefinition
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneDeployment(value *AgentDeployment) *AgentDeployment {
	if value == nil {
		return nil
	}
	var result AgentDeployment
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}
