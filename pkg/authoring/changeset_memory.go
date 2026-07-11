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
}

func NewMemoryChangeSetStore() *MemoryChangeSetStore {
	return &MemoryChangeSetStore{changeSets: map[string]*ChangeSet{}, idempotency: map[string]memoryChangeSetIdempotency{}}
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
