package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

func (s *MemoryStore) CreateAgentTurn(_ context.Context, turn *AgentTurn) (*AgentTurn, error) {
	if err := turn.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runKey := portfolioKey(turn.Scope, turn.RunID)
	if s.agentRuns[runKey] == nil {
		return nil, ErrRunNotFound
	}
	if s.turns[runKey] == nil {
		s.turns[runKey] = make(map[string]*AgentTurn)
	}
	for _, existing := range s.turns[runKey] {
		if existing.Status == AgentTurnStatusRunning {
			return nil, ErrActiveTurnExists
		}
	}
	if s.turns[runKey][turn.ID] != nil {
		return nil, ErrRevisionConflict
	}
	persisted := cloneAgentTurn(turn)
	persisted.Sequence = int64(len(s.turns[runKey]) + 1)
	s.turns[runKey][persisted.ID] = persisted
	return cloneAgentTurn(persisted), nil
}

func (s *MemoryStore) GetAgentTurn(_ context.Context, scope Scope, turnID string) (*AgentTurn, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, turns := range s.turns {
		if turn := turns[turnID]; turn != nil && turn.Scope == scope {
			return cloneAgentTurn(turn), nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) ListAgentTurns(_ context.Context, filter AgentTurnFilter) ([]*AgentTurn, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.RunID == "" {
		return nil, ErrRunNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	runKey := portfolioKey(filter.Scope, filter.RunID)
	result := make([]*AgentTurn, 0, len(s.turns[runKey]))
	for _, turn := range s.turns[runKey] {
		if turn.Sequence > filter.AfterSequence && matchesAgentTurnStatuses(turn.Status, filter.Statuses) {
			result = append(result, cloneAgentTurn(turn))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func matchesAgentTurnStatuses(status AgentTurnStatus, statuses []AgentTurnStatus) bool {
	if len(statuses) == 0 {
		return true
	}
	for _, candidate := range statuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func (s *MemoryStore) UpdateAgentTurn(_ context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error {
	if err := turn.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runKey := portfolioKey(turn.Scope, turn.RunID)
	current := s.turns[runKey][turn.ID]
	if current == nil {
		return ErrTurnNotFound
	}
	if current.Revision != expectedRevision || turn.Revision != expectedRevision+1 || current.Sequence != turn.Sequence {
		return ErrRevisionConflict
	}
	if current.LeaseOwner == "" || current.LeaseOwner != workerID {
		return ErrTurnLeaseHeld
	}
	s.turns[runKey][turn.ID] = cloneAgentTurn(turn)
	return nil
}

func (s *MemoryStore) ClaimAgentTurn(_ context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, turns := range s.turns {
		turn := turns[turnID]
		if turn == nil || turn.Scope != scope {
			continue
		}
		if turn.Status != AgentTurnStatusRunning {
			return nil, ErrTurnLeaseHeld
		}
		if turn.LeaseOwner != workerID && turn.LeaseExpiresAt != nil && turn.LeaseExpiresAt.After(now) {
			return nil, ErrTurnLeaseHeld
		}
		expires := now.Add(leaseDuration)
		turn.LeaseOwner = workerID
		turn.LeaseExpiresAt = &expires
		turn.UpdatedAt = now
		turn.Revision++
		return cloneAgentTurn(turn), nil
	}
	return nil, ErrTurnNotFound
}

func cloneAgentTurn(in *AgentTurn) *AgentTurn {
	if in == nil {
		return nil
	}
	var out AgentTurn
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}
