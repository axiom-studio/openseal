package runtime

import (
	"context"
	"errors"
	"sort"
)

func (s *MemoryStore) CreateRunbookActivation(_ context.Context, value *RunbookActivation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := value.Scope.key() + ":" + value.ID
	if _, exists := s.runbookActivations[key]; exists {
		return errors.New("Runbook activation already exists")
	}
	if value.IdempotencyKeyHash != "" {
		for _, existing := range s.runbookActivations {
			if existing.Scope == value.Scope && existing.IdempotencyKeyHash == value.IdempotencyKeyHash {
				if existing.CreationFingerprint == value.CreationFingerprint {
					return nil
				}
				return ErrRunbookActivationIdempotency
			}
		}
	}
	s.runbookActivations[key] = cloneRunbookActivation(value)
	return nil
}

func (s *MemoryStore) GetRunbookActivation(_ context.Context, scope Scope, id string) (*RunbookActivation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRunbookActivation(s.runbookActivations[scope.key()+":"+id]), nil
}

func (s *MemoryStore) ListRunbookActivations(_ context.Context, filter RunbookActivationFilter) ([]*RunbookActivation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]*RunbookActivation, 0)
	for _, value := range s.runbookActivations {
		if matchesRunbookActivationFilter(value, filter) {
			values = append(values, cloneRunbookActivation(value))
		}
	}
	return sortAndLimitRunbookActivations(values, filter.Limit, filter.Offset), nil
}

func (s *MemoryStore) UpdateRunbookActivation(_ context.Context, value *RunbookActivation, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := value.Scope.key() + ":" + value.ID
	existing := s.runbookActivations[key]
	if existing == nil {
		return ErrRunbookActivationNotFound
	}
	if existing.Revision != expectedRevision {
		return ErrRunbookActivationRevision
	}
	s.runbookActivations[key] = cloneRunbookActivation(value)
	return nil
}

func (s *MemoryStore) ListRunbookActivationScopes(_ context.Context) ([]Scope, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	unique := map[Scope]bool{}
	for _, value := range s.runbookActivations {
		unique[value.Scope] = true
	}
	result := make([]Scope, 0, len(unique))
	for scope := range unique {
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind == result[j].Kind {
			return result[i].ID < result[j].ID
		}
		return result[i].Kind < result[j].Kind
	})
	return result, nil
}
