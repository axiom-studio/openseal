package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

// Typed keys retain exact scope and provider identities without relying on a
// delimiter that callers may include in otherwise valid opaque identifiers.
type memoryRunEventRunKey struct {
	Scope Scope
	RunID string
}

type memoryRunEventWaitKey struct {
	memoryRunEventRunKey
	Key string
}

type memoryRunEventReceiptKey struct {
	Scope  Scope
	Source string
	ID     string
}

type memoryRunEventSelector struct {
	Scope   Scope
	Source  string
	Type    string
	Subject string
}

func memoryRunEventWaitSelector(wait *RunEventWait) memoryRunEventSelector {
	return memoryRunEventSelector{Scope: wait.Scope, Source: wait.Spec.Source, Type: wait.Spec.Type, Subject: wait.Spec.Subject}
}

func memoryRunEventReceiptSelector(receipt *RunEventReceipt) memoryRunEventSelector {
	event := receipt.Event
	return memoryRunEventSelector{Scope: event.Scope, Source: event.Source, Type: event.Type, Subject: event.Subject}
}

func memoryRunEventReceiptIdentity(receipt *RunEventReceipt) memoryRunEventReceiptKey {
	return memoryRunEventReceiptKey{Scope: receipt.Event.Scope, Source: receipt.Event.Source, ID: receipt.Event.ID}
}

// prepareMemoryRunEventWaitLocked validates the projection before any Run or
// Objective mutation. A wait key is immutable and cannot rearm a resolved wait.
func (s *MemoryStore) prepareMemoryRunEventWaitLocked(run *AgentRun) (*RunEventWait, error) {
	wait, err := runEventWaitForRun(run)
	if err != nil || wait == nil {
		return wait, err
	}
	key := memoryRunEventWaitKey{memoryRunEventRunKey{Scope: run.Scope, RunID: run.ID}, wait.Spec.Key}
	if existing := s.runEventWaits[key]; existing != nil {
		if !sameRunEventWaitSpec(&existing.Spec, &wait.Spec) || existing.AssignedAgentID != wait.AssignedAgentID ||
			(existing.Status != RunEventWaitPending && existing.Status != RunEventWaitPaused) {
			return nil, ErrInvalidRunEventWait
		}
	}
	return wait, nil
}

// syncMemoryRunEventWaitLocked commits a previously validated projection under
// the same mutex as the canonical Run and its activity record.
func (s *MemoryStore) syncMemoryRunEventWaitLocked(run *AgentRun, wait *RunEventWait) {
	runKey := memoryRunEventRunKey{Scope: run.Scope, RunID: run.ID}
	if previousKey, ok := s.runEventWaitActive[runKey]; ok {
		previous := s.runEventWaits[previousKey]
		if wait == nil || previousKey.Key != wait.Spec.Key {
			canceled := cloneRunEventWait(previous)
			canceled.Status = RunEventWaitCanceled
			canceled.AvailableAt, canceled.LeaseExpiresAt = nil, nil
			canceled.LeaseOwner = ""
			at := run.UpdatedAt
			canceled.ResolvedAt = &at
			s.saveMemoryRunEventWaitLocked(previousKey, canceled)
		}
	}
	if wait == nil {
		return
	}
	key := memoryRunEventWaitKey{runKey, wait.Spec.Key}
	if previous := s.runEventWaits[key]; previous != nil && previous.Status == wait.Status {
		// An unrelated Run update must not disturb a claimed wait or make an
		// idle wait poll again. Pause/resume changes deliberately reset leases.
		wait = cloneRunEventWait(previous)
	}
	s.saveMemoryRunEventWaitLocked(key, wait)
}

func (s *MemoryStore) saveMemoryRunEventWaitLocked(key memoryRunEventWaitKey, wait *RunEventWait) {
	if previous := s.runEventWaits[key]; previous != nil {
		selector := memoryRunEventWaitSelector(previous)
		delete(s.runEventWaitIndex[selector], key)
		if len(s.runEventWaitIndex[selector]) == 0 {
			delete(s.runEventWaitIndex, selector)
		}
		if active, exists := s.runEventWaitActive[key.memoryRunEventRunKey]; exists && active == key {
			delete(s.runEventWaitActive, key.memoryRunEventRunKey)
		}
		s.removeMemoryRunEventWaitOrderedIndexesLocked(key, previous)
	}
	s.runEventWaits[key] = cloneRunEventWait(wait)
	if wait.Status == RunEventWaitPending || wait.Status == RunEventWaitPaused {
		selector := memoryRunEventWaitSelector(wait)
		if s.runEventWaitIndex[selector] == nil {
			s.runEventWaitIndex[selector] = make(map[memoryRunEventWaitKey]struct{})
		}
		s.runEventWaitIndex[selector][key] = struct{}{}
		s.runEventWaitActive[key.memoryRunEventRunKey] = key
	}
	s.insertMemoryRunEventWaitOrderedIndexesLocked(key, wait)
	s.refreshMemoryRunEventScopeWorkLocked(key.Scope)
}

func (s *MemoryStore) ListRunEventWaitWorkScopes(_ context.Context, now time.Time, limit int) ([]Scope, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, ErrInvalidRunEventWait
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ready := memoryOrderedIndexAfter(s.runEventWaitScopeOrder, nil, func(key memoryRunEventScopeDueKey) bool {
		return !key.AvailableAt.After(now)
	}, limit, memoryRunEventScopeDueLess)
	scopes := make([]Scope, 0, len(ready))
	for _, key := range ready {
		scopes = append(scopes, key.Scope)
		available := now.Add(time.Second)
		s.setMemoryRunEventScopeWorkLocked(key.Scope, &available)
	}
	return scopes, nil
}

func (s *MemoryStore) PublishRunEvent(_ context.Context, receipt *RunEventReceipt) (bool, error) {
	if err := validateRunEventReceipt(receipt); err != nil {
		return false, err
	}
	stored, err := cloneMemoryRunEventReceipt(receipt)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishMemoryRunEventLocked(stored)
}

// publishMemoryRunEventLocked accepts a validated, detached observation. Other
// authenticated intake stores reuse it in their existing kernel transaction.
// It performs all identity conflict checks before any index or receipt writes.
func (s *MemoryStore) publishMemoryRunEventLocked(stored *RunEventReceipt) (bool, error) {
	key := memoryRunEventReceiptIdentity(stored)
	if existing := s.runEventReceipts[key]; existing != nil {
		if existing.Digest != stored.Digest {
			return false, ErrRunEventConflict
		}
		return false, nil
	}
	s.runEventReceipts[key] = stored
	s.insertMemoryRunEventReceiptOrderLocked(stored)
	selector := memoryRunEventReceiptSelector(stored)
	if s.runEventReceiptIndex[selector] == nil {
		s.runEventReceiptIndex[selector] = make(map[memoryRunEventReceiptKey]struct{})
	}
	s.runEventReceiptIndex[selector][key] = struct{}{}
	s.saveMemoryRunEventNotificationLocked(runEventNotificationForReceipt(stored))
	return true, nil
}

func (s *MemoryStore) ClaimRunEventWaits(_ context.Context, request ClaimRunEventWaitsRequest) ([]*RunEventWait, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyMemoryRunEventWaitsLocked(request.Scope, request.Now)
	due := memoryOrderedIndexAfter(s.runEventWaitDueOrder[request.Scope], nil, func(key memoryRunEventWaitDueKey) bool {
		return !key.AvailableAt.After(request.Now)
	}, request.Limit, memoryRunEventWaitDueLess)
	claimed := make([]*RunEventWait, 0, len(due))
	for _, key := range due {
		wait := cloneRunEventWait(s.runEventWaits[key.Wait])
		wait.LeaseOwner = request.WorkerID
		expires := request.Now.Add(request.LeaseDuration)
		wait.LeaseExpiresAt = &expires
		available := expires
		wait.AvailableAt = &available
		s.saveMemoryRunEventWaitLocked(key.Wait, wait)
		claimed = append(claimed, cloneRunEventWait(wait))
	}
	s.refreshMemoryRunEventScopeWorkLocked(request.Scope)
	return claimed, nil
}

func (s *MemoryStore) ProcessRunEventWait(_ context.Context, request ProcessRunEventWaitRequest) (*RunEventWaitResult, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runKey := memoryRunEventRunKey{Scope: request.Scope, RunID: request.RunID}
	key := memoryRunEventWaitKey{runKey, request.Key}
	wait := s.runEventWaits[key]
	if wait == nil {
		return nil, ErrRunEventWaitNotFound
	}
	if wait.Status != RunEventWaitPending || wait.LeaseOwner != request.WorkerID ||
		wait.LeaseExpiresAt == nil || !wait.LeaseExpiresAt.Equal(request.LeaseExpiresAt) || !wait.LeaseExpiresAt.After(request.Now) {
		return nil, ErrLeaseLost
	}
	run := s.agentRuns[portfolioKey(request.Scope, request.RunID)]
	if run == nil {
		return nil, ErrRunNotFound
	}
	var receipt *RunEventReceipt
	for eventKey := range s.runEventReceiptIndex[memoryRunEventWaitSelector(wait)] {
		candidate := s.runEventReceipts[eventKey]
		if _, consumed := s.runEventConsumptions[eventKey][runKey]; consumed || !runEventWaitMatches(wait, candidate) {
			continue
		}
		if receipt == nil || candidate.ReceivedAt.Before(receipt.ReceivedAt) ||
			(candidate.ReceivedAt.Equal(receipt.ReceivedAt) && candidate.Event.ID < receipt.Event.ID) {
			receipt = candidate
		}
	}
	updated, resolved, activity, err := prepareRunEventWaitResolution(run, wait, receipt, request.Now)
	if err != nil {
		return nil, err
	}
	var resultEvent *EventEnvelope
	if updated != nil {
		if err := updated.Validate(); err != nil {
			return nil, err
		}
		if err := activity.Validate(); err != nil {
			return nil, err
		}
		if receipt != nil {
			copy, err := cloneMemoryRunEventReceipt(receipt)
			if err != nil {
				return nil, err
			}
			resultEvent = &copy.Event
		}
	}
	// The result, event consumption, canonical Run and audit event become
	// visible together. Authority changed by another store path wins here.
	s.saveMemoryRunEventWaitLocked(key, resolved)
	s.refreshMemoryRunEventScopeWorkLocked(request.Scope)
	if updated != nil {
		if receipt != nil {
			eventKey := memoryRunEventReceiptIdentity(receipt)
			if s.runEventConsumptions[eventKey] == nil {
				s.runEventConsumptions[eventKey] = make(map[memoryRunEventRunKey]struct{})
			}
			s.runEventConsumptions[eventKey][runKey] = struct{}{}
		}
		s.saveMemoryAgentRunLocked(portfolioKey(updated.Scope, updated.ID), updated)
		activity = appendMemoryActivityLocked(s, activity)
	}
	return &RunEventWaitResult{Wait: cloneRunEventWait(resolved), Run: cloneAgentRun(updated), Event: resultEvent, Activity: cloneActivityEvent(activity)}, nil
}

func (s *MemoryStore) GetRunEventWait(_ context.Context, scope Scope, runID, key string) (*RunEventWait, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(runID, 128) || !validOpaqueIdentifier(key, 256) {
		return nil, ErrInvalidRunEventWait
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	wait := s.runEventWaits[memoryRunEventWaitKey{memoryRunEventRunKey{Scope: scope, RunID: runID}, key}]
	if wait == nil {
		return nil, ErrRunEventWaitNotFound
	}
	return cloneRunEventWait(wait), nil
}

func (s *MemoryStore) PruneRunEvents(_ context.Context, scope Scope, now time.Time, limit int) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if now.IsZero() || limit < 1 || limit > 1000 {
		return 0, ErrInvalidRunEventWait
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.memoryRunEventReceiptsAfterLocked(s.runEventReceiptPruneCursors[scope], now.Add(-RunEventRetention), limit, &scope)
	deleted := 0
	for _, key := range batch {
		receipt := s.runEventReceipts[key.Identity]
		if !s.memoryRunEventReceiptProtectedLocked(receipt) {
			s.deleteMemoryRunEventReceiptLocked(key.Identity)
			deleted++
		}
	}
	if len(batch) < limit {
		delete(s.runEventReceiptPruneCursors, scope)
	} else {
		last := batch[len(batch)-1]
		s.runEventReceiptPruneCursors[scope] = &last
	}
	return deleted, nil
}

func cloneMemoryRunEventReceipt(receipt *RunEventReceipt) (*RunEventReceipt, error) {
	data, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var cloned RunEventReceipt
	if err := decoder.Decode(&cloned); err != nil {
		return nil, err
	}
	return &cloned, nil
}

var _ RunEventWaitStore = (*MemoryStore)(nil)
