package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
)

// MemoryStore holds execution records in memory with a configurable max size.
type MemoryStore struct {
	mu       sync.RWMutex
	runs     map[int]*RunRecord
	maxSize  int
	nextID   int
}

// NewMemoryStore creates an in-memory store for execution records.
func NewMemoryStore(maxSize int) *MemoryStore {
	if maxSize <= 0 {
		maxSize = 100
	}
	return &MemoryStore{
		runs:    make(map[int]*RunRecord),
		maxSize: maxSize,
		nextID:  1,
	}
}

// CreateRun implements ExecutionStore.
func (s *MemoryStore) CreateRun(_ context.Context, workflowName string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.runs[id] = &RunRecord{
		RunID:        id,
		WorkflowName: workflowName,
		Status:       "pending",
		NodeResults:  make(map[string]*executor.NodeResult),
		StartedAt:    time.Now(),
	}
	s.evictIfNeeded()
	return id, nil
}

// GetRun implements ExecutionStore.
func (s *MemoryStore) GetRun(_ context.Context, runID int) (*RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, nil
	}
	return r, nil
}

// UpdateNodeResult implements ExecutionStore.
func (s *MemoryStore) UpdateNodeResult(_ context.Context, runID int, nodeID string, result *executor.NodeResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[runID]; ok {
		r.NodeResults[nodeID] = result
	}
	return nil
}

// UpdateRunStatus implements ExecutionStore.
func (s *MemoryStore) UpdateRunStatus(_ context.Context, runID int, status string, runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[runID]; ok {
		r.Status = status
		now := time.Now()
		r.CompletedAt = &now
		if runErr != nil {
			r.Error = runErr.Error()
		}
	}
	return nil
}

// IncrementRetryCount implements ExecutionStore.
func (s *MemoryStore) IncrementRetryCount(_ context.Context, runID int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[runID]; ok {
		r.RetryCount++
		return r.RetryCount, nil
	}
	return 0, nil
}

// ListRuns implements ExecutionStore.
func (s *MemoryStore) ListRuns(_ context.Context, limit int) ([]*RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*RunRecord, 0, len(s.runs))
	for _, r := range s.runs {
		list = append(list, r)
	}
	// Sort by RunID descending (newest first)
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	if limit > 0 && limit < len(list) {
		list = list[:limit]
	}
	return list, nil
}

func (s *MemoryStore) evictIfNeeded() {
	for len(s.runs) > s.maxSize {
		var oldest int
		var oldestTime time.Time
		first := true
		for id, r := range s.runs {
			if first || r.StartedAt.Before(oldestTime) {
				oldest = id
				oldestTime = r.StartedAt
				first = false
			}
		}
		delete(s.runs, oldest)
	}
}
