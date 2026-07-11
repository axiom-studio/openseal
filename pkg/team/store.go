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
	CreateTeamDefinition(context.Context, *Definition) error
	GetTeamDefinition(context.Context, string, string) (*Definition, error)
	ListTeamDefinitionVersions(context.Context, string) ([]*Definition, error)
	CreateTeamDeployment(context.Context, *Deployment, workforce.DefinitionActivation) error
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*Deployment, error)
	UpdateTeamDeployment(context.Context, *Deployment, int64, workforce.DefinitionActivation) error
	ListTeamDefinitionActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error)
	CreateTeamAmendment(context.Context, *DefinitionAmendment) error
	GetTeamAmendment(context.Context, capability.ScopeReference, string) (*DefinitionAmendment, error)
	UpdateTeamAmendment(context.Context, *DefinitionAmendment, int64) error
	ActivateTeamAmendment(context.Context, *DefinitionAmendment, int64, *Definition, *Deployment, int64, workforce.DefinitionActivation) error
}

type MemoryStore struct {
	mu          sync.RWMutex
	definitions map[string]*Definition
	deployments map[string]*Deployment
	activations map[string][]workforce.DefinitionActivation
	amendments  map[string]*DefinitionAmendment
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		definitions: make(map[string]*Definition), deployments: make(map[string]*Deployment),
		activations: make(map[string][]workforce.DefinitionActivation), amendments: make(map[string]*DefinitionAmendment),
	}
}

func (s *MemoryStore) CreateTeamAmendment(_ context.Context, amendment *DefinitionAmendment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := amendmentKey(amendment.Scope, amendment.ID)
	if s.amendments[key] != nil {
		return errors.New("team definition amendment already exists")
	}
	s.amendments[key] = cloneAmendment(amendment)
	return nil
}

func (s *MemoryStore) GetTeamAmendment(_ context.Context, scope capability.ScopeReference, id string) (*DefinitionAmendment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.amendments[amendmentKey(scope, id)]
	if value == nil {
		return nil, ErrAmendmentNotFound
	}
	return cloneAmendment(value), nil
}

func (s *MemoryStore) UpdateTeamAmendment(_ context.Context, amendment *DefinitionAmendment, expectedRevision int64) error {
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

func (s *MemoryStore) ActivateTeamAmendment(_ context.Context, amendment *DefinitionAmendment, expectedAmendmentRevision int64, definition *Definition, deployment *Deployment, expectedDeploymentRevision int64, activation workforce.DefinitionActivation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	amendmentStorageKey := amendmentKey(amendment.Scope, amendment.ID)
	currentAmendment := s.amendments[amendmentStorageKey]
	deploymentStorageKey := deploymentKey(deployment.Scope, deployment.ID)
	currentDeployment := s.deployments[deploymentStorageKey]
	if currentAmendment == nil || currentDeployment == nil {
		return errors.New("team amendment or deployment not found")
	}
	if currentAmendment.Revision != expectedAmendmentRevision || currentDeployment.Revision != expectedDeploymentRevision {
		return ErrRevisionConflict
	}
	definitionStorageKey := definitionKey(definition.ID, definition.Version)
	if s.definitions[definitionStorageKey] != nil {
		return errors.New("team definition versions are immutable")
	}
	s.definitions[definitionStorageKey] = cloneDefinition(definition)
	s.deployments[deploymentStorageKey] = cloneDeployment(deployment)
	s.amendments[amendmentStorageKey] = cloneAmendment(amendment)
	s.activations[deploymentStorageKey] = append(s.activations[deploymentStorageKey], activation)
	return nil
}

func (s *MemoryStore) CreateTeamDefinition(_ context.Context, definition *Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := definitionKey(definition.ID, definition.Version)
	if s.definitions[key] != nil {
		return errors.New("team definition versions are immutable")
	}
	s.definitions[key] = cloneDefinition(definition)
	return nil
}

func (s *MemoryStore) GetTeamDefinition(_ context.Context, id, version string) (*Definition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.definitions[definitionKey(id, version)]
	if value == nil {
		return nil, ErrDefinitionNotFound
	}
	return cloneDefinition(value), nil
}

func (s *MemoryStore) ListTeamDefinitionVersions(_ context.Context, id string) ([]*Definition, error) {
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

func (s *MemoryStore) CreateTeamDeployment(_ context.Context, deployment *Deployment, activation workforce.DefinitionActivation) error {
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

func (s *MemoryStore) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.deployments[deploymentKey(scope, id)]
	if value == nil {
		return nil, ErrDeploymentNotFound
	}
	return cloneDeployment(value), nil
}

func (s *MemoryStore) UpdateTeamDeployment(_ context.Context, deployment *Deployment, expectedRevision int64, activation workforce.DefinitionActivation) error {
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

func (s *MemoryStore) ListTeamDefinitionActivations(_ context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := s.activations[deploymentKey(scope, deploymentID)]
	result := make([]workforce.DefinitionActivation, len(values))
	copy(result, values)
	return result, nil
}
