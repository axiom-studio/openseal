package authoring

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type memoryChangeSetIdempotency struct {
	RequestDigest string
	ChangeSetID   string
}

type MemoryChangeSetStore struct {
	mu          sync.RWMutex
	changeSets  map[string]*ChangeSet
	idempotency map[string]memoryChangeSetIdempotency
	definitions map[string]string
	deployments map[string]AppliedResourceReference
	objectives  map[string]AppliedResourceReference
}

func NewMemoryChangeSetStore() *MemoryChangeSetStore {
	return &MemoryChangeSetStore{changeSets: map[string]*ChangeSet{}, idempotency: map[string]memoryChangeSetIdempotency{}, definitions: map[string]string{}, deployments: map[string]AppliedResourceReference{}, objectives: map[string]AppliedResourceReference{}}
}

func (s *MemoryChangeSetStore) ApplyChangeSet(_ context.Context, value *ChangeSet, expectedRevision int64) (*ChangeSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := changeSetKey(value.Scope, value.ID)
	current := s.changeSets[key]
	if current == nil {
		return nil, ErrChangeSetNotFound
	}
	if current.Status == ChangeSetApplied && current.ApplyReceipt != nil && value.ApplyReceipt != nil && current.ApplyReceipt.IdempotencyKey == value.ApplyReceipt.IdempotencyKey {
		return cloneChangeSet(current), nil
	}
	if current.Revision != expectedRevision || current.Status != ChangeSetReady || current.CandidateDigest != value.CandidateDigest {
		return nil, ErrChangeSetRevision
	}
	resources, definitions, deployments, objectives, err := buildMemoryApplication(current)
	if err != nil {
		return nil, err
	}
	for k := range definitions {
		if s.definitions[k] != "" {
			return nil, ErrChangeSetRevision
		}
	}
	for k, proposed := range deployments {
		currentDeployment, exists := s.deployments[k]
		if current.Mode == ModeCreate && exists {
			return nil, ErrChangeSetRevision
		}
		if current.Mode == ModeAmend && (!exists || currentDeployment.Revision+1 != proposed.Revision) {
			return nil, ErrChangeSetRevision
		}
	}
	for k, proposed := range objectives {
		currentObjective, exists := s.objectives[k]
		if proposed.Revision == 1 && exists || proposed.Revision > 1 && (!exists || currentObjective.Revision+1 != proposed.Revision) {
			return nil, ErrChangeSetRevision
		}
	}
	for k, digest := range definitions {
		s.definitions[k] = digest
	}
	for k, deployment := range deployments {
		s.deployments[k] = deployment
	}
	for k, objective := range objectives {
		s.objectives[k] = objective
	}
	copy := cloneChangeSet(value)
	copy.ApplyReceipt.Resources = resources
	s.changeSets[key] = copy
	return cloneChangeSet(copy), nil
}

func buildMemoryApplication(value *ChangeSet) ([]AppliedResourceReference, map[string]string, map[string]AppliedResourceReference, map[string]AppliedResourceReference, error) {
	definitions := map[string]string{}
	deployments := map[string]AppliedResourceReference{}
	objectives := map[string]AppliedResourceReference{}
	resources := make([]AppliedResourceReference, 0)
	for _, definition := range value.Result.Candidate.Agents {
		digest, _ := digestJSON(definition)
		ref := AppliedResourceReference{Kind: "agent_definition", ID: definition.ID, Version: definition.Version}
		definitions["agent\x00"+definition.ID+"\x00"+definition.Version] = digest
		resources = append(resources, ref)
		revision := int64(1)
		if value.Mode == ModeAmend {
			revision = value.Placement.AgentExpectedRevisions[definition.ID] + 1
		}
		deployment := AppliedResourceReference{Kind: "agent_deployment", ID: value.Placement.AgentDeploymentIDs[definition.ID], Version: definition.Version, Revision: revision}
		deployments[changeSetKey(value.Scope, deployment.ID)] = deployment
		resources = append(resources, deployment)
		for _, template := range definition.ObjectiveTemplates {
			placement := value.Placement.Objectives[WorkforceObjectiveKey("agent", definition.ID, template.ID)]
			ref := AppliedResourceReference{Kind: "objective", ID: placement.ID, Revision: placement.ExpectedRevision + 1}
			objectives[changeSetKey(value.Scope, ref.ID)] = ref
			resources = append(resources, ref)
		}
	}
	if value.Result.Candidate.Team == nil {
		return resources, definitions, deployments, objectives, nil
	}
	team := value.Result.Candidate.Team
	digest, _ := digestJSON(team)
	definitions["team\x00"+team.ID+"\x00"+team.Version] = digest
	resources = append(resources, AppliedResourceReference{Kind: "team_definition", ID: team.ID, Version: team.Version})
	revision := int64(1)
	if value.Mode == ModeAmend {
		revision = value.Placement.TeamExpectedRevision + 1
	}
	deployment := AppliedResourceReference{Kind: "team_deployment", ID: value.Placement.TeamDeploymentID, Version: team.Version, Revision: revision}
	deployments[changeSetKey(value.Scope, deployment.ID)] = deployment
	resources = append(resources, deployment)
	for _, template := range team.ObjectiveTemplates {
		placement := value.Placement.Objectives[WorkforceObjectiveKey("team", team.ID, template.ID)]
		ref := AppliedResourceReference{Kind: "objective", ID: placement.ID, Revision: placement.ExpectedRevision + 1}
		objectives[changeSetKey(value.Scope, ref.ID)] = ref
		resources = append(resources, ref)
	}
	return resources, definitions, deployments, objectives, nil
}

func (s *MemoryChangeSetStore) GetChangeSetByIdempotency(_ context.Context, scope capability.ScopeReference, key, requestDigest string) (*ChangeSet, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	existing, ok := s.idempotency[changeSetKey(scope, key)]
	if !ok {
		return nil, false, nil
	}
	if existing.RequestDigest != requestDigest {
		return nil, false, ErrChangeSetIdempotency
	}
	return cloneChangeSet(s.changeSets[changeSetKey(scope, existing.ChangeSetID)]), true, nil
}

func (s *MemoryChangeSetStore) CreateChangeSet(_ context.Context, value *ChangeSet, key, requestDigest string) (*ChangeSet, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idempotencyKey := changeSetKey(value.Scope, key)
	if existing, ok := s.idempotency[idempotencyKey]; ok {
		if existing.RequestDigest != requestDigest {
			return nil, false, ErrChangeSetIdempotency
		}
		return cloneChangeSet(s.changeSets[changeSetKey(value.Scope, existing.ChangeSetID)]), true, nil
	}
	copy := cloneChangeSet(value)
	s.changeSets[changeSetKey(value.Scope, value.ID)] = copy
	s.idempotency[idempotencyKey] = memoryChangeSetIdempotency{RequestDigest: requestDigest, ChangeSetID: value.ID}
	return cloneChangeSet(copy), false, nil
}

func (s *MemoryChangeSetStore) GetChangeSet(_ context.Context, scope capability.ScopeReference, id string) (*ChangeSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.changeSets[changeSetKey(scope, id)]
	if value == nil {
		return nil, ErrChangeSetNotFound
	}
	return cloneChangeSet(value), nil
}

func (s *MemoryChangeSetStore) UpdateChangeSet(_ context.Context, value *ChangeSet, expectedRevision int64) (*ChangeSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := changeSetKey(value.Scope, value.ID)
	current := s.changeSets[key]
	if current == nil {
		return nil, ErrChangeSetNotFound
	}
	if current.Revision != expectedRevision || current.CandidateDigest != value.CandidateDigest {
		return nil, ErrChangeSetRevision
	}
	copy := cloneChangeSet(value)
	s.changeSets[key] = copy
	return cloneChangeSet(copy), nil
}

func changeSetKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}

func cloneChangeSet(value *ChangeSet) *ChangeSet {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	var copy ChangeSet
	_ = json.Unmarshal(payload, &copy)
	return &copy
}
