package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
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
	store Store
	now   func() time.Time
	newID func() string
}

func NewRegistry() *Registry {
	return NewRegistryWithStore(NewMemoryStore())
}

func NewRegistryWithStore(store Store) *Registry {
	return &Registry{store: store, now: time.Now, newID: uuid.NewString}
}

func (r *Registry) RegisterDefinition(ctx context.Context, definition *AgentDefinition) (*AgentDefinition, error) {
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
	if err := r.store.CreateDefinition(ctx, candidate); err != nil {
		return nil, err
	}
	return cloneDefinition(candidate), nil
}

func (r *Registry) GetDefinition(ctx context.Context, id, version string) (*AgentDefinition, error) {
	return r.store.GetDefinition(ctx, id, version)
}

func (r *Registry) ListDefinitionVersions(ctx context.Context, id string) ([]*AgentDefinition, error) {
	return r.store.ListDefinitionVersions(ctx, id)
}

func (r *Registry) CreateDeployment(ctx context.Context, deployment *AgentDeployment, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
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
	definition, err := r.store.GetDefinition(ctx, candidate.DefinitionID, candidate.ActiveVersion)
	if err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, candidate); err != nil {
		return nil, nil, err
	}
	activation := DefinitionActivation{
		ID: r.newID(), Scope: candidate.Scope, DeploymentID: candidate.ID, DefinitionID: candidate.DefinitionID,
		ToVersion: candidate.ActiveVersion, DeploymentRevision: candidate.Revision, Reason: strings.TrimSpace(reason),
		ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: candidate.CreatedAt,
	}
	if err := r.store.CreateDeployment(ctx, candidate, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(candidate), &copyActivation, nil
}

func (r *Registry) GetDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*AgentDeployment, error) {
	return r.store.GetDeployment(ctx, scope, id)
}

func (r *Registry) ActivateDefinition(ctx context.Context, scope capability.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("deployment activation actor is required")
	}
	current, err := r.store.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	if current.ActiveVersion == version {
		return nil, nil, errors.New("agent deployment already uses the requested definition version")
	}
	definition, err := r.store.GetDefinition(ctx, current.DefinitionID, version)
	if err != nil {
		return nil, nil, err
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
	if err := r.store.UpdateDeployment(ctx, updated, expectedRevision, activation); err != nil {
		return nil, nil, err
	}
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

func (r *Registry) ListActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]DefinitionActivation, error) {
	return r.store.ListActivations(ctx, scope, deploymentID)
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
