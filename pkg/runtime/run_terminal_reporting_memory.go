package runtime

import (
	"container/heap"
	"context"
	"time"
)

type runTerminalReportKey struct {
	Scope  Scope
	RunID  string
	Status AgentRunStatus
}

type memoryTerminalReportDue struct {
	key runTerminalReportKey
	at  time.Time
}

// Two indexed heaps support both host-global and scope-specific bounded claims
// without enumerating Runs, delivered reports, or dormant tenant inventories.
type memoryTerminalReportHeap struct {
	items     []memoryTerminalReportDue
	positions map[runTerminalReportKey]int
}

func (h memoryTerminalReportHeap) Len() int { return len(h.items) }
func (h memoryTerminalReportHeap) Less(i, j int) bool {
	left, right := h.items[i], h.items[j]
	if !left.at.Equal(right.at) {
		return left.at.Before(right.at)
	}
	if left.key.Scope.Kind != right.key.Scope.Kind {
		return left.key.Scope.Kind < right.key.Scope.Kind
	}
	if left.key.Scope.ID != right.key.Scope.ID {
		return left.key.Scope.ID < right.key.Scope.ID
	}
	if left.key.RunID != right.key.RunID {
		return left.key.RunID < right.key.RunID
	}
	return left.key.Status < right.key.Status
}
func (h memoryTerminalReportHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.positions[h.items[i].key], h.positions[h.items[j].key] = i, j
}
func (h *memoryTerminalReportHeap) Push(value interface{}) {
	item := value.(memoryTerminalReportDue)
	if h.positions == nil {
		h.positions = make(map[runTerminalReportKey]int)
	}
	h.positions[item.key] = len(h.items)
	h.items = append(h.items, item)
}
func (h *memoryTerminalReportHeap) Pop() interface{} {
	last := len(h.items) - 1
	item := h.items[last]
	h.items[last] = memoryTerminalReportDue{}
	h.items = h.items[:last]
	delete(h.positions, item.key)
	return item
}
func (h *memoryTerminalReportHeap) set(key runTerminalReportKey, at time.Time) {
	if index, found := h.positions[key]; found {
		h.items[index].at = at
		heap.Fix(h, index)
		return
	}
	heap.Push(h, memoryTerminalReportDue{key: key, at: at})
}
func (h *memoryTerminalReportHeap) remove(key runTerminalReportKey) {
	if index, found := h.positions[key]; found {
		heap.Remove(h, index)
	}
}

type memoryTerminalReportQueue struct {
	global memoryTerminalReportHeap
	scopes map[Scope]*memoryTerminalReportHeap
}

func (q *memoryTerminalReportQueue) set(key runTerminalReportKey, at time.Time) {
	q.global.set(key, at)
	if q.scopes == nil {
		q.scopes = make(map[Scope]*memoryTerminalReportHeap)
	}
	if q.scopes[key.Scope] == nil {
		q.scopes[key.Scope] = &memoryTerminalReportHeap{}
	}
	q.scopes[key.Scope].set(key, at)
}
func (q *memoryTerminalReportQueue) remove(key runTerminalReportKey) {
	q.global.remove(key)
	if queue := q.scopes[key.Scope]; queue != nil {
		queue.remove(key)
		if queue.Len() == 0 {
			delete(q.scopes, key.Scope)
		}
	}
}

// All in-memory canonical Run writes use this commit boundary while holding
// MemoryStore.mu. Report enqueue is therefore atomic with specialized action,
// approval, dependency and ordinary command transitions alike.
func (s *MemoryStore) saveMemoryAgentRunLocked(key string, run *AgentRun) {
	previous := s.agentRuns[key]
	wasActive := previous != nil && !isTerminalAgentRunStatus(previous.Status)
	s.agentRuns[key] = cloneAgentRun(run)
	s.refreshMemoryConversationTaskRunLocked(run)
	s.refreshMemoryConversationActiveRunLocked(run)
	s.refreshMemoryConversationActiveRootLocked(key, previous, run)
	s.refreshMemoryConversationForegroundRunLocked(run)
	s.refreshMemoryConversationTaskDueRunLocked(run)
	s.refreshMemorySkillRuntimeMaintenanceWaiterLocked(key)
	s.refreshMemoryRunSkillDependenciesLocked(key, run)
	if wasActive != !isTerminalAgentRunStatus(run.Status) {
		s.refreshMemorySkillRuntimeReceiptsLocked(key)
	}
	if !runNeedsTerminalReporting(run) {
		return
	}
	reportKey := runTerminalReportKey{Scope: run.Scope, RunID: run.ID, Status: run.Status}
	if s.runTerminalReports[reportKey] != nil {
		return
	}
	if s.runTerminalReports == nil {
		s.runTerminalReports = make(map[runTerminalReportKey]*RunTerminalReport)
	}
	available := run.UpdatedAt
	if available.IsZero() {
		available = run.CreatedAt
	}
	s.runTerminalReports[reportKey] = &RunTerminalReport{Scope: run.Scope, RunID: run.ID, Status: run.Status, Run: cloneAgentRun(run), AvailableAt: available}
	s.runTerminalReportQueue.set(reportKey, available)
}

func (s *MemoryStore) ClaimRunTerminalReports(ctx context.Context, request RunTerminalReportingClaim) ([]*RunTerminalReport, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := &s.runTerminalReportQueue.global
	if request.Scope != nil {
		queue = s.runTerminalReportQueue.scopes[*request.Scope]
	}
	results := make([]*RunTerminalReport, 0, request.Limit)
	expires := request.Now.Add(request.LeaseDuration)
	for queue != nil && queue.Len() > 0 && len(results) < request.Limit {
		item := queue.items[0]
		if item.at.After(request.Now) {
			break
		}
		report := s.runTerminalReports[item.key]
		report.LeaseOwner, report.LeaseExpiresAt = request.WorkerID, &expires
		report.Attempts++
		report.AvailableAt = expires
		s.runTerminalReportQueue.set(item.key, expires)
		results = append(results, cloneRunTerminalReport(report))
	}
	return results, nil
}

func (s *MemoryStore) CompleteRunTerminalReport(ctx context.Context, request RunTerminalReportingCompletion) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := runTerminalReportKey{Scope: request.Scope, RunID: request.RunID, Status: request.Status}
	report, err := s.leasedMemoryTerminalReportLocked(key, request)
	if err != nil {
		return err
	}
	at := request.Now
	report.DeliveredAt, report.Run = &at, nil
	report.LeaseOwner, report.LeaseExpiresAt = "", nil
	s.runTerminalReportQueue.remove(key)
	return nil
}

func (s *MemoryStore) RetryRunTerminalReport(ctx context.Context, request RunTerminalReportingRetry) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := runTerminalReportKey{Scope: request.Scope, RunID: request.RunID, Status: request.Status}
	report, err := s.leasedMemoryTerminalReportLocked(key, request.RunTerminalReportingCompletion)
	if err != nil {
		return err
	}
	report.AvailableAt = request.AvailableAt
	report.LeaseOwner, report.LeaseExpiresAt = "", nil
	s.runTerminalReportQueue.set(key, report.AvailableAt)
	return nil
}

func (s *MemoryStore) leasedMemoryTerminalReportLocked(key runTerminalReportKey, request RunTerminalReportingCompletion) (*RunTerminalReport, error) {
	report := s.runTerminalReports[key]
	if report == nil || report.DeliveredAt != nil || report.LeaseOwner != request.WorkerID || report.LeaseExpiresAt == nil || !report.LeaseExpiresAt.Equal(request.LeaseExpiresAt) || !report.LeaseExpiresAt.After(request.Now) {
		return nil, ErrLeaseLost
	}
	return report, nil
}

func (s *MemoryStore) GetRunTerminalReport(ctx context.Context, scope Scope, runID string, status AgentRunStatus) (*RunTerminalReport, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(runID, 128) || !isTerminalAgentRunStatus(status) {
		return nil, ErrInvalidRunTerminalReport
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	report := s.runTerminalReports[runTerminalReportKey{Scope: scope, RunID: runID, Status: status}]
	if report == nil {
		return nil, ErrRunTerminalReportNotFound
	}
	return cloneRunTerminalReport(report), nil
}
