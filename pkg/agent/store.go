package agent

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type Store interface {
	CreateDefinition(context.Context, *AgentDefinition) error
	GetDefinition(context.Context, string, string) (*AgentDefinition, error)
	ListDefinitionVersions(context.Context, string) ([]*AgentDefinition, error)
	CreateDeployment(context.Context, *AgentDeployment, DefinitionActivation) error
	GetDeployment(context.Context, capability.ScopeReference, string) (*AgentDeployment, error)
	UpdateDeployment(context.Context, *AgentDeployment, int64, DefinitionActivation) error
	ListActivations(context.Context, capability.ScopeReference, string) ([]DefinitionActivation, error)
}

type MemoryStore struct {
	mu          sync.RWMutex
	definitions map[string]*AgentDefinition
	deployments map[string]*AgentDeployment
	activations map[string][]DefinitionActivation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{definitions: make(map[string]*AgentDefinition), deployments: make(map[string]*AgentDeployment), activations: make(map[string][]DefinitionActivation)}
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
