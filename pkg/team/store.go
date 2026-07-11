package team

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type Store interface {
	CreateDefinition(context.Context, *Definition) error
	GetDefinition(context.Context, string, string) (*Definition, error)
	ListDefinitionVersions(context.Context, string) ([]*Definition, error)
	CreateDeployment(context.Context, *Deployment, workforce.DefinitionActivation) error
	GetDeployment(context.Context, capability.ScopeReference, string) (*Deployment, error)
	UpdateDeployment(context.Context, *Deployment, int64, workforce.DefinitionActivation) error
	ListActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error)
}

type MemoryStore struct {
	mu          sync.RWMutex
	definitions map[string]*Definition
	deployments map[string]*Deployment
	activations map[string][]workforce.DefinitionActivation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		definitions: make(map[string]*Definition), deployments: make(map[string]*Deployment),
		activations: make(map[string][]workforce.DefinitionActivation),
	}
}

func (s *MemoryStore) CreateDefinition(_ context.Context, definition *Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := definitionKey(definition.ID, definition.Version)
	if s.definitions[key] != nil {
		return errors.New("team definition versions are immutable")
	}
	s.definitions[key] = cloneDefinition(definition)
	return nil
}

func (s *MemoryStore) GetDefinition(_ context.Context, id, version string) (*Definition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.definitions[definitionKey(id, version)]
	if value == nil {
		return nil, ErrDefinitionNotFound
	}
	return cloneDefinition(value), nil
}

func (s *MemoryStore) ListDefinitionVersions(_ context.Context, id string) ([]*Definition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Definition, 0)
	for _, value := range s.definitions {
		if value.ID == id {
			result = append(result, cloneDefinition(value))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	return result, nil
}

func (s *MemoryStore) CreateDeployment(_ context.Context, deployment *Deployment, activation workforce.DefinitionActivation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := deploymentKey(deployment.Scope, deployment.ID)
	if s.deployments[key] != nil {
		return errors.New("team deployment already exists")
	}
	s.deployments[key] = cloneDeployment(deployment)
	s.activations[key] = append(s.activations[key], activation)
	return nil
}

func (s *MemoryStore) GetDeployment(_ context.Context, scope capability.ScopeReference, id string) (*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.deployments[deploymentKey(scope, id)]
	if value == nil {
		return nil, ErrDeploymentNotFound
	}
	return cloneDeployment(value), nil
}

func (s *MemoryStore) UpdateDeployment(_ context.Context, deployment *Deployment, expectedRevision int64, activation workforce.DefinitionActivation) error {
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

func (s *MemoryStore) ListActivations(_ context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := s.activations[deploymentKey(scope, deploymentID)]
	result := make([]workforce.DefinitionActivation, len(values))
	copy(result, values)
	return result, nil
}
