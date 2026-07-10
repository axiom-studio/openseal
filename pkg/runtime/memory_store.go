package runtime

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
)

// MemoryStore holds execution records in memory with a configurable max size.
type MemoryStore struct {
	mu                  sync.RWMutex
	runs                map[int]*RunRecord
	objectives          map[string]*Objective
	agentRuns           map[string]*AgentRun
	activity            map[string][]*ActivityEvent
	turns               map[string]map[string]*AgentTurn
	actions             map[string]*ActionCall
	approvals           map[string]*ApprovalCheckpoint
	actionKeys          map[string]string
	requests            map[string]*AgentRequest
	requestKeys         map[string]string
	dependencyGroups    map[string]*RunDependencyGroup
	dependencyGroupKeys map[string]string
	dependencies        map[string]map[string]*RunDependency
	artifacts           map[string]map[int64]*Artifact
	maxSize             int
	nextID              int
}

// NewMemoryStore creates an in-memory store for execution records.
func NewMemoryStore(maxSize int) *MemoryStore {
	if maxSize <= 0 {
		maxSize = 100
	}
	return &MemoryStore{
		runs:                make(map[int]*RunRecord),
		objectives:          make(map[string]*Objective),
		agentRuns:           make(map[string]*AgentRun),
		activity:            make(map[string][]*ActivityEvent),
		turns:               make(map[string]map[string]*AgentTurn),
		actions:             make(map[string]*ActionCall),
		approvals:           make(map[string]*ApprovalCheckpoint),
		actionKeys:          make(map[string]string),
		requests:            make(map[string]*AgentRequest),
		requestKeys:         make(map[string]string),
		dependencyGroups:    make(map[string]*RunDependencyGroup),
		dependencyGroupKeys: make(map[string]string),
		dependencies:        make(map[string]map[string]*RunDependency),
		artifacts:           make(map[string]map[int64]*Artifact),
		maxSize:             maxSize,
		nextID:              1,
	}
}

// CreateRun implements ExecutionStore.
func (s *MemoryStore) CreateRun(_ context.Context, workflow WorkflowEntry, triggerData map[string]interface{}) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	id := s.nextID
	s.nextID++
	s.runs[id] = &RunRecord{
		RunID:        id,
		WorkflowName: workflow.Name,
		Workflow:     workflow,
		TriggerData:  cloneMap(triggerData),
		Status:       RunStatusPending,
		NodeResults:  make(map[string]*executor.NodeResult),
		CreatedAt:    now,
		UpdatedAt:    now,
		AvailableAt:  now,
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
	return cloneRunRecord(r), nil
}

func (s *MemoryStore) ClaimNextRunnable(_ context.Context, workerID string, leaseDuration time.Duration) (*RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	ids := make([]int, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		r := s.runs[id]
		runnable := (r.Status == RunStatusPending || r.Status == RunStatusRetrying) && !r.AvailableAt.After(now)
		expired := r.Status == RunStatusRunning && r.LeaseExpiresAt != nil && !r.LeaseExpiresAt.After(now)
		if !runnable && !expired {
			continue
		}
		expires := now.Add(leaseDuration)
		r.Status = RunStatusRunning
		r.LeaseOwner = workerID
		r.LeaseExpiresAt = &expires
		r.UpdatedAt = now
		if r.StartedAt == nil {
			started := now
			r.StartedAt = &started
		}
		return cloneRunRecord(r), nil
	}
	return nil, nil
}

func (s *MemoryStore) RenewLease(_ context.Context, runID int, workerID string, leaseDuration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || r.Status != RunStatusRunning || r.LeaseOwner != workerID {
		return ErrLeaseLost
	}
	now := time.Now()
	expires := now.Add(leaseDuration)
	r.LeaseExpiresAt = &expires
	r.UpdatedAt = now
	return nil
}

func (s *MemoryStore) UpdateNodeResult(_ context.Context, runID int, workerID, nodeID string, result *executor.NodeResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || r.Status != RunStatusRunning || r.LeaseOwner != workerID {
		return ErrLeaseLost
	}
	r.NodeResults[nodeID] = result
	r.UpdatedAt = time.Now()
	return nil
}

func (s *MemoryStore) CompleteRun(_ context.Context, runID int, workerID, status string, runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || r.Status != RunStatusRunning || r.LeaseOwner != workerID {
		return ErrLeaseLost
	}
	now := time.Now()
	r.Status = status
	r.UpdatedAt = now
	r.CompletedAt = &now
	r.LeaseOwner = ""
	r.LeaseExpiresAt = nil
	if runErr != nil {
		r.Error = runErr.Error()
	} else {
		r.Error = ""
	}
	return nil
}

func (s *MemoryStore) ScheduleRetry(_ context.Context, runID int, workerID string, availableAt time.Time, runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || r.Status != RunStatusRunning || r.LeaseOwner != workerID {
		return ErrLeaseLost
	}
	r.Status = RunStatusRetrying
	r.RetryCount++
	r.AvailableAt = availableAt
	r.UpdatedAt = time.Now()
	r.LeaseOwner = ""
	r.LeaseExpiresAt = nil
	if runErr != nil {
		r.Error = runErr.Error()
	}
	return nil
}

// ListRuns implements ExecutionStore.
func (s *MemoryStore) ListRuns(_ context.Context, limit int) ([]*RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*RunRecord, 0, len(s.runs))
	for _, r := range s.runs {
		list = append(list, cloneRunRecord(r))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].RunID > list[j].RunID })
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
			if first || r.CreatedAt.Before(oldestTime) {
				oldest = id
				oldestTime = r.CreatedAt
				first = false
			}
		}
		delete(s.runs, oldest)
	}
}

func cloneRunRecord(in *RunRecord) *RunRecord {
	if in == nil {
		return nil
	}
	out := *in
	out.TriggerData = cloneMap(in.TriggerData)
	out.NodeResults = make(map[string]*executor.NodeResult, len(in.NodeResults))
	for key, result := range in.NodeResults {
		out.NodeResults[key] = result
	}
	return &out
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
