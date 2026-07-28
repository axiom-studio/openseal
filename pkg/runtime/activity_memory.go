package runtime

import (
	"context"
	"encoding/json"
)

func (s *MemoryStore) CreateObjectiveWithEvent(_ context.Context, objective *Objective, event *ActivityEvent) (*ActivityEvent, error) {
	if err := objective.Validate(); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if objective.Scope != event.Scope || objective.ID != event.ObjectiveID || event.RunID != "" {
		return nil, ErrInvalidScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(objective.Scope, objective.ID)
	if s.objectives[key] != nil {
		return nil, ErrObjectiveIdempotency
	}
	s.objectives[key] = cloneObjective(objective)
	persisted := appendMemoryActivityLocked(s, event)
	return cloneActivityEvent(persisted), nil
}

func (s *MemoryStore) UpdateObjectiveWithEvent(_ context.Context, objective *Objective, expectedRevision int64, event *ActivityEvent) (*ActivityEvent, error) {
	if err := objective.Validate(); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if objective.Scope != event.Scope || objective.ID != event.ObjectiveID || event.RunID != "" {
		return nil, ErrInvalidScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(objective.Scope, objective.ID)
	current := s.objectives[key]
	if current == nil {
		return nil, ErrObjectiveNotFound
	}
	if current.Revision != expectedRevision || objective.Revision != expectedRevision+1 {
		return nil, ErrRevisionConflict
	}
	s.objectives[key] = cloneObjective(objective)
	persisted := appendMemoryActivityLocked(s, event)
	return cloneActivityEvent(persisted), nil
}

func (s *MemoryStore) CreateAgentRunWithEvent(_ context.Context, run *AgentRun, event *ActivityEvent) (*ActivityEvent, error) {
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
	if s.agentRuns[key] != nil {
		return nil, ErrRunIdempotency
	}
	if err := s.allocateMemoryObjectiveRunLocked(run); err != nil {
		return nil, err
	}
	s.agentRuns[key] = cloneAgentRun(run)
	persisted := appendMemoryActivityLocked(s, event)
	return cloneActivityEvent(persisted), nil
}

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
	persisted := appendMemoryActivityLocked(s, event)
	s.agentRuns[key] = cloneAgentRun(run)
	return cloneActivityEvent(persisted), nil
}

func (s *MemoryStore) AppendActivity(_ context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.RunID != "" && s.agentRuns[portfolioKey(event.Scope, event.RunID)] == nil {
		return nil, ErrRunNotFound
	}
	if event.RunID == "" && event.ProjectID == "" && s.objectives[portfolioKey(event.Scope, event.ObjectiveID)] == nil {
		return nil, ErrObjectiveNotFound
	}
	if event.ProjectID != "" && s.projects[projectKey(event.Scope, event.ProjectID)] == nil {
		return nil, ErrProjectNotFound
	}
	persisted := appendMemoryActivityLocked(s, event)
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
