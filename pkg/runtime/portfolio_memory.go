package runtime

import (
	"context"
	"encoding/json"
	"sort"
)

func portfolioKey(scope Scope, id string) string { return scope.key() + ":" + id }

func (s *MemoryStore) CreateObjective(_ context.Context, objective *Objective) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(objective.Scope, objective.ID)
	if s.objectives[key] != nil {
		return ErrObjectiveIdempotency
	}
	s.objectives[key] = cloneObjective(objective)
	return nil
}

func (s *MemoryStore) GetObjective(_ context.Context, scope Scope, objectiveID string) (*Objective, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneObjective(s.objectives[portfolioKey(scope, objectiveID)]), nil
}

func (s *MemoryStore) ListObjectives(_ context.Context, filter ObjectiveFilter) ([]*Objective, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Objective, 0)
	for _, objective := range s.objectives {
		if objective.Scope != filter.Scope || !matchesObjectiveFilter(objective, filter) {
			continue
		}
		result = append(result, cloneObjective(objective))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		if !result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return pageObjectives(result, filter.Offset, filter.Limit), nil
}

func (s *MemoryStore) ListObjectiveScopes(_ context.Context) ([]Scope, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[Scope]struct{})
	for _, objective := range s.objectives {
		seen[objective.Scope] = struct{}{}
	}
	result := make([]Scope, 0, len(seen))
	for scope := range seen {
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (s *MemoryStore) UpdateObjective(_ context.Context, objective *Objective, expectedRevision int64) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(objective.Scope, objective.ID)
	current := s.objectives[key]
	if current == nil {
		return ErrObjectiveNotFound
	}
	if current.Revision != expectedRevision || objective.Revision != expectedRevision+1 {
		return ErrRevisionConflict
	}
	s.objectives[key] = cloneObjective(objective)
	return nil
}

func (s *MemoryStore) CreateAgentRun(_ context.Context, run *AgentRun) error {
	if err := run.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(run.Scope, run.ID)
	if s.agentRuns[key] != nil {
		return ErrRunIdempotency
	}
	if err := s.allocateMemoryObjectiveRunLocked(run); err != nil {
		return err
	}
	s.agentRuns[key] = cloneAgentRun(run)
	return nil
}

func (s *MemoryStore) allocateMemoryObjectiveRunLocked(run *AgentRun) error {
	if run == nil || run.ObjectiveID == "" || run.ParentRunID != "" {
		return nil
	}
	key := portfolioKey(run.Scope, run.ObjectiveID)
	objective := s.objectives[key]
	if objective == nil {
		return ErrObjectiveNotFound
	}
	updated, err := allocateObjectiveRunBudget(objective, run, run.CreatedAt)
	if err != nil {
		return err
	}
	if updated != objective {
		s.objectives[key] = cloneObjective(updated)
	}
	return nil
}

func (s *MemoryStore) GetAgentRun(_ context.Context, scope Scope, runID string) (*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAgentRun(s.agentRuns[portfolioKey(scope, runID)]), nil
}

func (s *MemoryStore) ListAgentRuns(_ context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*AgentRun, 0)
	for _, run := range s.agentRuns {
		if run.Scope != filter.Scope || !matchesRunFilter(run, filter) {
			continue
		}
		result = append(result, cloneAgentRun(run))
	}
	sortAgentRuns(result, filter.Order)
	return pageAgentRuns(result, filter.Offset, filter.Limit), nil
}

func (s *MemoryStore) SummarizeAgentRuns(_ context.Context, scope Scope, owners []ObjectiveOwner) ([]AgentRunOwnerSummary, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	requested := make(map[string]ObjectiveOwner, len(owners))
	for _, owner := range owners {
		requested[string(owner.Type)+"\x1f"+owner.ID] = owner
	}
	summaries := make(map[string]AgentRunOwnerSummary, len(owners))
	for _, run := range s.agentRuns {
		if run.Scope != scope {
			continue
		}
		key := string(run.Owner.Type) + "\x1f" + run.Owner.ID
		owner, ok := requested[key]
		if !ok {
			continue
		}
		summary := summaries[key]
		summary.Owner = owner
		summary.RunCount++
		if summary.LastRunAt == nil || run.CreatedAt.After(*summary.LastRunAt) {
			at := run.CreatedAt
			summary.LastRunAt = &at
			summary.LastStatus = run.Status
		}
		summaries[key] = summary
	}
	result := make([]AgentRunOwnerSummary, 0, len(summaries))
	for _, owner := range owners {
		if summary, ok := summaries[string(owner.Type)+"\x1f"+owner.ID]; ok {
			result = append(result, summary)
		}
	}
	return result, nil
}

func matchesObjectiveFilter(objective *Objective, filter ObjectiveFilter) bool {
	if filter.Owner != nil && objective.Owner != *filter.Owner {
		return false
	}
	if !filter.IncludeRetired && objective.Status == ObjectiveStatusRetired {
		return false
	}
	if len(filter.Statuses) > 0 && !containsObjectiveStatus(filter.Statuses, objective.Status) {
		return false
	}
	return true
}

func matchesRunFilter(run *AgentRun, filter AgentRunFilter) bool {
	if filter.Kind != "" && normalizeRunKind(run.Kind) != filter.Kind {
		return false
	}
	if filter.Owner != nil && run.Owner != *filter.Owner {
		return false
	}
	if filter.ObjectiveID != "" && run.ObjectiveID != filter.ObjectiveID {
		return false
	}
	if filter.ParentRunID != "" && run.ParentRunID != filter.ParentRunID {
		return false
	}
	if filter.RootRunID != "" && run.RootRunID != filter.RootRunID {
		return false
	}
	if filter.AssignedAgentID != "" && run.AssignedAgentID != filter.AssignedAgentID {
		return false
	}
	if len(filter.Statuses) > 0 && !containsRunStatus(filter.Statuses, run.Status) {
		return false
	}
	return true
}

func containsObjectiveStatus(statuses []ObjectiveStatus, status ObjectiveStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func containsRunStatus(statuses []AgentRunStatus, status AgentRunStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func pageObjectives(values []*Objective, offset, limit int) []*Objective {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*Objective{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}

func pageAgentRuns(values []*AgentRun, offset, limit int) []*AgentRun {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*AgentRun{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}

func cloneObjective(in *Objective) *Objective {
	if in == nil {
		return nil
	}
	var out Objective
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}

func cloneObjectiveExecutionPolicy(in *ObjectiveExecutionPolicy) *ObjectiveExecutionPolicy {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneAgentRun(in *AgentRun) *AgentRun {
	if in == nil {
		return nil
	}
	var out AgentRun
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	out.Kind = normalizeRunKind(out.Kind)
	return &out
}
