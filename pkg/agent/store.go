package agent

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type Store interface {
	CreateCompilation(context.Context, *DefinitionCompilation) error
	GetCompilation(context.Context, capability.ScopeReference, string) (*DefinitionCompilation, error)
	ListCompilations(context.Context, capability.ScopeReference, string) ([]*DefinitionCompilation, error)
	CreateDefinition(context.Context, *AgentDefinition) error
	GetDefinition(context.Context, string, string) (*AgentDefinition, error)
	ListDefinitionVersions(context.Context, string) ([]*AgentDefinition, error)
	CreateDeployment(context.Context, *AgentDeployment, DefinitionActivation) error
	GetDeployment(context.Context, capability.ScopeReference, string) (*AgentDeployment, error)
	ListDeployments(context.Context, capability.ScopeReference) ([]*AgentDeployment, error)
	UpdateDeployment(context.Context, *AgentDeployment, int64, DefinitionActivation) error
	ListActivations(context.Context, capability.ScopeReference, string) ([]DefinitionActivation, error)
	CreateAmendment(context.Context, *DefinitionAmendment) error
	GetAmendment(context.Context, capability.ScopeReference, string) (*DefinitionAmendment, error)
	UpdateAmendment(context.Context, *DefinitionAmendment, int64) error
	ActivateAmendment(context.Context, *DefinitionAmendment, int64, *AgentDefinition, *AgentDeployment, int64, DefinitionActivation) error
}

type MemoryStore struct {
	mu           sync.RWMutex
	definitions  map[string]*AgentDefinition
	deployments  map[string]*AgentDeployment
	activations  map[string][]DefinitionActivation
	amendments   map[string]*DefinitionAmendment
	compilations map[string]*DefinitionCompilation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{definitions: make(map[string]*AgentDefinition), deployments: make(map[string]*AgentDeployment), activations: make(map[string][]DefinitionActivation), amendments: make(map[string]*DefinitionAmendment), compilations: make(map[string]*DefinitionCompilation)}
}

func (s *MemoryStore) CreateCompilation(_ context.Context, compilation *DefinitionCompilation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compilationKey(compilation.Scope, compilation.ID)
	if s.compilations[key] != nil {
		return ErrCompilationImmutable
	}
	s.compilations[key] = cloneCompilation(compilation)
	return nil
}

func (s *MemoryStore) GetCompilation(_ context.Context, scope capability.ScopeReference, id string) (*DefinitionCompilation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.compilations[compilationKey(scope, id)]
	if value == nil {
		return nil, ErrCompilationNotFound
	}
	return cloneCompilation(value), nil
}

func (s *MemoryStore) ListCompilations(_ context.Context, scope capability.ScopeReference, deploymentID string) ([]*DefinitionCompilation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*DefinitionCompilation, 0)
	for _, value := range s.compilations {
		if value.Scope == scope && value.DeploymentID == deploymentID {
			result = append(result, cloneCompilation(value))
		}
	}
	sortCompilations(result)
	return result, nil
}

func (s *MemoryStore) CreateAmendment(_ context.Context, amendment *DefinitionAmendment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := amendmentKey(amendment.Scope, amendment.ID)
	if s.amendments[key] != nil {
		return errors.New("agent definition amendment already exists")
	}
	s.amendments[key] = cloneAmendment(amendment)
	return nil
}

func (s *MemoryStore) GetAmendment(_ context.Context, scope capability.ScopeReference, id string) (*DefinitionAmendment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.amendments[amendmentKey(scope, id)]
	if value == nil {
		return nil, ErrAmendmentNotFound
	}
	return cloneAmendment(value), nil
}

func (s *MemoryStore) UpdateAmendment(_ context.Context, amendment *DefinitionAmendment, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := amendmentKey(amendment.Scope, amendment.ID)
	current := s.amendments[key]
	if current == nil {
		return ErrAmendmentNotFound
	}
	if current.Revision != expectedRevision {
		return ErrRevisionConflict
	}
	s.amendments[key] = cloneAmendment(amendment)
	return nil
}

func (s *MemoryStore) ActivateAmendment(_ context.Context, amendment *DefinitionAmendment, expectedAmendmentRevision int64, definition *AgentDefinition, deployment *AgentDeployment, expectedDeploymentRevision int64, activation DefinitionActivation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	amendmentStorageKey := amendmentKey(amendment.Scope, amendment.ID)
	currentAmendment := s.amendments[amendmentStorageKey]
	deploymentStorageKey := deploymentKey(deployment.Scope, deployment.ID)
	currentDeployment := s.deployments[deploymentStorageKey]
	if currentAmendment == nil || currentDeployment == nil {
		return errors.New("amendment or deployment not found")
	}
	if currentAmendment.Revision != expectedAmendmentRevision || currentDeployment.Revision != expectedDeploymentRevision {
		return ErrRevisionConflict
	}
	definitionStorageKey := definitionKey(definition.ID, definition.Version)
	if s.definitions[definitionStorageKey] != nil {
		return errors.New("agent definition versions are immutable")
	}
	s.definitions[definitionStorageKey] = cloneDefinition(definition)
	s.deployments[deploymentStorageKey] = cloneDeployment(deployment)
	s.activations[deploymentStorageKey] = append(s.activations[deploymentStorageKey], activation)
	s.amendments[amendmentStorageKey] = cloneAmendment(amendment)
	return nil
}

func (s *MemoryStore) CreateDefinition(_ context.Context, definition *AgentDefinition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := definitionKey(definition.ID, definition.Version)
	if s.definitions[key] != nil {
		return errors.New("agent definition versions are immutable")
	}
	s.definitions[key] = cloneDefinition(definition)
	return nil
}

func (s *MemoryStore) GetDefinition(_ context.Context, id, version string) (*AgentDefinition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.definitions[definitionKey(id, version)]
	if value == nil {
		return nil, ErrDefinitionNotFound
	}
	return cloneDefinition(value), nil
}

func (s *MemoryStore) ListDefinitionVersions(_ context.Context, id string) ([]*AgentDefinition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*AgentDefinition, 0)
	for _, definition := range s.definitions {
		if definition.ID == id {
			result = append(result, cloneDefinition(definition))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	return result, nil
}

func (s *MemoryStore) CreateDeployment(_ context.Context, deployment *AgentDeployment, activation DefinitionActivation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := deploymentKey(deployment.Scope, deployment.ID)
	if s.deployments[key] != nil {
		return errors.New("agent deployment already exists")
	}
	s.deployments[key] = cloneDeployment(deployment)
	s.activations[key] = append(s.activations[key], activation)
	return nil
}

func (s *MemoryStore) GetDeployment(_ context.Context, scope capability.ScopeReference, id string) (*AgentDeployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.deployments[deploymentKey(scope, id)]
	if value == nil {
		return nil, ErrDeploymentNotFound
	}
	return cloneDeployment(value), nil
}

func (s *MemoryStore) ListDeployments(_ context.Context, scope capability.ScopeReference) ([]*AgentDeployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*AgentDeployment, 0)
	for _, value := range s.deployments {
		if value.Scope == scope {
			result = append(result, cloneDeployment(value))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (s *MemoryStore) UpdateDeployment(_ context.Context, deployment *AgentDeployment, expectedRevision int64, activation DefinitionActivation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := deploymentKey(deployment.Scope, deployment.ID)
	current := s.deployments[key]
	if current == nil {
		return ErrDeploymentNotFound
	}
	if current.Revision != expectedRevision {
		return ErrRevisionConflict
	}
	s.deployments[key] = cloneDeployment(deployment)
	s.activations[key] = append(s.activations[key], activation)
	return nil
}

func (s *MemoryStore) ListActivations(_ context.Context, scope capability.ScopeReference, deploymentID string) ([]DefinitionActivation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := s.activations[deploymentKey(scope, deploymentID)]
	result := make([]DefinitionActivation, len(values))
	copy(result, values)
	return result, nil
}
