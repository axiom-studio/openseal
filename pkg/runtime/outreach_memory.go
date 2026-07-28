package runtime

import (
	"context"
	"sort"
)

func outreachThreadKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}

func outreachIdempotencyKey(scope Scope, hash string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + hash
}

func (s *MemoryStore) CreateOutreachThreadWithEvent(_ context.Context, thread *OutreachThread, event *ActivityEvent) (*ActivityEvent, error) {
	if err := thread.Validate(); err != nil || event == nil || event.Validate() != nil {
		return nil, ErrInvalidOutreachThread
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := outreachThreadKey(thread.Scope, thread.ID)
	if s.outreachThreads[key] != nil {
		return nil, ErrOutreachThreadConflict
	}
	if thread.IdempotencyKeyHash != "" {
		idempotencyKey := outreachIdempotencyKey(thread.Scope, thread.IdempotencyKeyHash)
		if s.outreachIdempotency[idempotencyKey] != "" {
			return nil, ErrOutreachThreadIdempotency
		}
		s.outreachIdempotency[idempotencyKey] = thread.ID
	}
	s.outreachThreads[key] = cloneOutreachThread(thread)
	persisted := appendMemoryActivityLocked(s, event)
	return cloneActivityEvent(persisted), nil
}

func (s *MemoryStore) GetOutreachThread(_ context.Context, scope Scope, id string) (*OutreachThread, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.outreachThreads[outreachThreadKey(scope, id)]
	if value == nil {
		return nil, ErrOutreachThreadNotFound
	}
	return cloneOutreachThread(value), nil
}

func (s *MemoryStore) GetOutreachThreadByIdempotency(_ context.Context, scope Scope, hash string) (*OutreachThread, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.outreachIdempotency[outreachIdempotencyKey(scope, hash)]
	value := s.outreachThreads[outreachThreadKey(scope, id)]
	if value == nil {
		return nil, ErrOutreachThreadNotFound
	}
	return cloneOutreachThread(value), nil
}

func (s *MemoryStore) ListOutreachThreads(_ context.Context, filter OutreachThreadFilter) ([]*OutreachThread, error) {
	if err := filter.Scope.Validate(); err != nil || filter.Limit > 100 || filter.Offset < 0 {
		return nil, ErrInvalidOutreachThread
	}
	statuses := make(map[OutreachThreadStatus]bool, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses[status] = true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]*OutreachThread, 0)
	for _, value := range s.outreachThreads {
		if value.Scope != filter.Scope || filter.ProjectID != "" && value.ProjectID != filter.ProjectID ||
			filter.SourceObservationID != "" && value.SourceObservationID != filter.SourceObservationID || len(statuses) > 0 && !statuses[value.Status] {
			continue
		}
		values = append(values, cloneOutreachThread(value))
	}
	sort.Slice(values, func(i, j int) bool { return values[i].UpdatedAt.After(values[j].UpdatedAt) })
	start := min(filter.Offset, len(values))
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	end := min(start+limit, len(values))
	return values[start:end], nil
}

func (s *MemoryStore) UpdateOutreachThreadWithEvent(_ context.Context, thread *OutreachThread, expected int64, event *ActivityEvent) (*ActivityEvent, error) {
	if err := thread.Validate(); err != nil || event == nil || event.Validate() != nil || thread.Revision != expected+1 {
		return nil, ErrInvalidOutreachThread
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := outreachThreadKey(thread.Scope, thread.ID)
	current := s.outreachThreads[key]
	if current == nil {
		return nil, ErrOutreachThreadNotFound
	}
	if current.Revision != expected {
		return nil, ErrOutreachThreadConflict
	}
	s.outreachThreads[key] = cloneOutreachThread(thread)
	persisted := appendMemoryActivityLocked(s, event)
	return cloneActivityEvent(persisted), nil
}

var _ OutreachStore = (*MemoryStore)(nil)
