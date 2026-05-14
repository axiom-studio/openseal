package server

import (
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
)

// RunRecord stores the result of a workflow execution.
type RunRecord struct {
	RunID        int                      `json:"runId"`
	WorkflowName string                   `json:"workflowName"`
	Status       string                   `json:"status"`
	NodeResults  map[string]*executor.NodeResult `json:"nodeResults,omitempty"`
	StartedAt    time.Time                `json:"startedAt"`
	CompletedAt  *time.Time               `json:"completedAt,omitempty"`
	Error        string                   `json:"error,omitempty"`
}

// RunStore holds execution records in memory with a configurable max size.
type RunStore struct {
	mu       sync.RWMutex
	runs     map[int]*RunRecord
	maxSize  int
	nextID   int
}

// NewRunStore creates an in-memory store for execution records.
func NewRunStore(maxSize int) *RunStore {
	if maxSize <= 0 {
		maxSize = 100
	}
	return &RunStore{
		runs:    make(map[int]*RunRecord),
		maxSize: maxSize,
		nextID:  1,
	}
}

// Create initializes a new run record and returns its ID.
func (s *RunStore) Create(workflowName string) int {
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
	return id
}

// UpdateNodeResult stores the result of a single node execution.
func (s *RunStore) UpdateNodeResult(runID int, nodeID string, result *executor.NodeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[runID]; ok {
		r.NodeResults[nodeID] = result
	}
}

// Complete marks a run as completed or failed.
func (s *RunStore) Complete(runID int, status string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[runID]; ok {
		r.Status = status
		now := time.Now()
		r.CompletedAt = &now
		if err != nil {
			r.Error = err.Error()
		}
	}
}

// Get retrieves a run record by ID.
func (s *RunStore) Get(runID int) (*RunRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[runID]
	return r, ok
}

// List returns all runs, newest first.
func (s *RunStore) List() []*RunRecord {
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
	return list
}

func (s *RunStore) evictIfNeeded() {
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
