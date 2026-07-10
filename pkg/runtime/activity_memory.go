package runtime

import (
	"context"
	"encoding/json"
)

func (s *MemoryStore) UpdateAgentRunWithEvent(_ context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error) {
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if run.Scope != event.Scope || run.ID != event.RunID {
		return nil, ErrInvalidScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(run.Scope, run.ID)
	current := s.agentRuns[key]
	if current == nil {
		return nil, ErrRunNotFound
	}
	if current.Revision != expectedRevision || run.Revision != expectedRevision+1 {
		return nil, ErrRevisionConflict
	}
	if lease != nil && (current.LeaseOwner != lease.WorkerID || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(lease.Now)) {
		return nil, ErrLeaseLost
	}
	persisted := cloneActivityEvent(event)
	persisted.Sequence = int64(len(s.activity[key]) + 1)
	s.agentRuns[key] = cloneAgentRun(run)
	s.activity[key] = append(s.activity[key], persisted)
	return cloneActivityEvent(persisted), nil
}

func (s *MemoryStore) AppendActivity(_ context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(event.Scope, event.RunID)
	if s.agentRuns[key] == nil {
		return nil, ErrRunNotFound
	}
	persisted := cloneActivityEvent(event)
	persisted.Sequence = int64(len(s.activity[key]) + 1)
	s.activity[key] = append(s.activity[key], persisted)
	return cloneActivityEvent(persisted), nil
}

func (s *MemoryStore) ListActivity(_ context.Context, filter ActivityFilter) ([]*ActivityEvent, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if !filter.Descending {
		key := portfolioKey(filter.Scope, filter.RunID)
		if s.agentRuns[key] == nil {
			return []*ActivityEvent{}, nil
		}
		result := make([]*ActivityEvent, 0, limit)
		for _, event := range s.activity[key] {
			if event.Sequence <= filter.AfterSequence || !matchesActivityFilter(event, filter) {
				continue
			}
			result = append(result, cloneActivityEvent(event))
			if len(result) == limit {
				break
			}
		}
		return result, nil
	}
	result := make([]*ActivityEvent, 0)
	for _, events := range s.activity {
		for _, event := range events {
			if matchesActivityFilter(event, filter) {
				result = append(result, cloneActivityEvent(event))
			}
		}
	}
	sortActivityEvents(result, true)
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func cloneActivityEvent(in *ActivityEvent) *ActivityEvent {
	if in == nil {
		return nil
	}
	var out ActivityEvent
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}
