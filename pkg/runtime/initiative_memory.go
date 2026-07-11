package runtime

import (
	"context"
	"sort"
)

func initiativeKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}
func (s *MemoryStore) CreateInitiative(_ context.Context, i *Initiative) error {
	if err := i.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := initiativeKey(i.Scope, i.ID)
	if _, ok := s.initiatives[k]; ok {
		return ErrInitiativeConflict
	}
	s.initiatives[k] = cloneInitiative(i)
	return nil
}
func (s *MemoryStore) CreateInitiativeWithEvent(_ context.Context, i *Initiative, e *ActivityEvent) (*ActivityEvent, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := initiativeKey(i.Scope, i.ID)
	if s.initiatives[k] != nil {
		return nil, ErrInitiativeConflict
	}
	if i.IdempotencyKeyHash != "" {
		for _, v := range s.initiatives {
			if v.Scope == i.Scope && v.IdempotencyKeyHash == i.IdempotencyKeyHash {
				return nil, ErrInitiativeIdempotency
			}
		}
	}
	s.initiatives[k] = cloneInitiative(i)
	p := appendMemoryActivityLocked(s, e)
	return cloneActivityEvent(p), nil
}
func (s *MemoryStore) GetInitiative(_ context.Context, scope Scope, id string) (*Initiative, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := s.initiatives[initiativeKey(scope, id)]
	if i == nil {
		return nil, ErrInitiativeNotFound
	}
	return cloneInitiative(i), nil
}
func (s *MemoryStore) GetInitiativeByIdempotency(_ context.Context, scope Scope, key string) (*Initiative, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, i := range s.initiatives {
		if i.Scope == scope && i.IdempotencyKeyHash == key {
			return cloneInitiative(i), nil
		}
	}
	return nil, ErrInitiativeNotFound
}
func (s *MemoryStore) ListInitiatives(_ context.Context, f InitiativeFilter) ([]*Initiative, error) {
	if err := f.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := map[InitiativeStatus]bool{}
	for _, v := range f.Statuses {
		status[v] = true
	}
	out := []*Initiative{}
	for _, i := range s.initiatives {
		if i.Scope != f.Scope {
			continue
		}
		if f.Owner != nil && i.Owner != *f.Owner {
			continue
		}
		if len(status) > 0 && !status[i.Status] {
			continue
		}
		if f.ObjectiveID != "" && !containsString(i.ObjectiveRefs, f.ObjectiveID) {
			continue
		}
		out = append(out, cloneInitiative(i))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].UpdatedAt.After(out[b].UpdatedAt) })
	start := f.Offset
	if start > len(out) {
		start = len(out)
	}
	end := len(out)
	if f.Limit > 0 && start+f.Limit < end {
		end = start + f.Limit
	}
	return out[start:end], nil
}
func (s *MemoryStore) UpdateInitiativeWithEvent(_ context.Context, i *Initiative, expected int64, e *ActivityEvent) (*ActivityEvent, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := initiativeKey(i.Scope, i.ID)
	cur := s.initiatives[k]
	if cur == nil {
		return nil, ErrInitiativeNotFound
	}
	if cur.Revision != expected || i.Revision != expected+1 {
		return nil, ErrInitiativeConflict
	}
	s.initiatives[k] = cloneInitiative(i)
	p := appendMemoryActivityLocked(s, e)
	return cloneActivityEvent(p), nil
}
func (s *MemoryStore) UpdateInitiative(_ context.Context, i *Initiative, expected int64) error {
	if err := i.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := initiativeKey(i.Scope, i.ID)
	current := s.initiatives[k]
	if current == nil {
		return ErrInitiativeNotFound
	}
	if current.Revision != expected || i.Revision != expected+1 {
		return ErrInitiativeConflict
	}
	s.initiatives[k] = cloneInitiative(i)
	return nil
}
func initiativeContainsString(v []string, w string) bool {
	for _, s := range v {
		if s == w {
			return true
		}
	}
	return false
}

var _ InitiativeStore = (*MemoryStore)(nil)
