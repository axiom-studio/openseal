package runtime

import (
	"context"
	"encoding/json"
	"sort"
)

func (s *MemoryStore) CreateActionProposal(_ context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	if err := validateActionProposalRecord(proposal); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	call := proposal.Call
	if call.IdempotencyKey != "" {
		key := actionIdempotencyKey(call.Scope, call.RunID, call.IdempotencyKey)
		if existingID := s.actionKeys[key]; existingID != "" {
			existing := s.actions[portfolioKey(call.Scope, existingID)]
			if existing.InvocationDigest != call.InvocationDigest {
				return nil, ErrIdempotencyConflict
			}
			return &ActionProposalResult{
				Call: cloneActionCall(existing), Approval: cloneApprovalCheckpoint(s.approvals[portfolioKey(call.Scope, existing.ApprovalID)]),
				Created: false,
			}, nil
		}
	}
	runKey := portfolioKey(call.Scope, call.RunID)
	currentRun := s.agentRuns[runKey]
	if currentRun == nil {
		return nil, ErrRunNotFound
	}
	if currentRun.Revision != proposal.ExpectedRunRevision || proposal.Run.Revision != proposal.ExpectedRunRevision+1 {
		return nil, ErrRevisionConflict
	}
	if proposal.Lease != nil && (currentRun.LeaseOwner != proposal.Lease.WorkerID || currentRun.LeaseExpiresAt == nil || !currentRun.LeaseExpiresAt.After(proposal.Lease.Now)) {
		return nil, ErrLeaseLost
	}
	if s.actions[portfolioKey(call.Scope, call.ID)] != nil {
		return nil, ErrRevisionConflict
	}
	if proposal.Approval != nil && s.approvals[portfolioKey(call.Scope, proposal.Approval.ID)] != nil {
		return nil, ErrRevisionConflict
	}
	event := cloneActivityEvent(proposal.Event)
	event.Sequence = int64(len(s.activity[runKey]) + 1)
	s.actions[portfolioKey(call.Scope, call.ID)] = cloneActionCall(call)
	if call.IdempotencyKey != "" {
		s.actionKeys[actionIdempotencyKey(call.Scope, call.RunID, call.IdempotencyKey)] = call.ID
	}
	if proposal.Approval != nil {
		s.approvals[portfolioKey(call.Scope, proposal.Approval.ID)] = cloneApprovalCheckpoint(proposal.Approval)
	}
	s.agentRuns[runKey] = cloneAgentRun(proposal.Run)
	s.activity[runKey] = append(s.activity[runKey], event)
	return &ActionProposalResult{
		Call: cloneActionCall(call), Approval: cloneApprovalCheckpoint(proposal.Approval),
		Event: cloneActivityEvent(event), Created: true,
	}, nil
}

func (s *MemoryStore) GetActionCall(_ context.Context, scope Scope, actionID string) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	call := s.actions[portfolioKey(scope, actionID)]
	if call == nil {
		return nil, ErrActionNotFound
	}
	return cloneActionCall(call), nil
}

func (s *MemoryStore) ListActionCalls(_ context.Context, filter ActionFilter) ([]*ActionCall, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ActionCall, 0)
	for _, call := range s.actions {
		if call.Scope != filter.Scope || filter.RunID != "" && call.RunID != filter.RunID ||
			len(filter.Status) > 0 && !containsActionStatus(filter.Status, call.Status) {
			continue
		}
		result = append(result, cloneActionCall(call))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return pageActionCalls(result, filter.Offset, filter.Limit), nil
}

func (s *MemoryStore) GetApproval(_ context.Context, scope Scope, approvalID string) (*ApprovalCheckpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	approval := s.approvals[portfolioKey(scope, approvalID)]
	if approval == nil {
		return nil, ErrApprovalNotFound
	}
	return cloneApprovalCheckpoint(approval), nil
}

func (s *MemoryStore) ListApprovals(_ context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ApprovalCheckpoint, 0)
	for _, approval := range s.approvals {
		if approval.Scope != filter.Scope || filter.RunID != "" && approval.RunID != filter.RunID ||
			len(filter.Status) > 0 && !containsApprovalStatus(filter.Status, approval.Status) {
			continue
		}
		result = append(result, cloneApprovalCheckpoint(approval))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return pageApprovals(result, filter.Offset, filter.Limit), nil
}

func validateActionProposalRecord(proposal ActionProposalRecord) error {
	if err := proposal.Call.Validate(); err != nil {
		return err
	}
	if err := proposal.Run.Validate(); err != nil {
		return err
	}
	if err := proposal.Event.Validate(); err != nil {
		return err
	}
	if proposal.Call.Scope != proposal.Run.Scope || proposal.Call.Scope != proposal.Event.Scope ||
		proposal.Call.RunID != proposal.Run.ID || proposal.Call.RunID != proposal.Event.RunID {
		return ErrInvalidScope
	}
	if proposal.Approval != nil {
		if err := proposal.Approval.Validate(); err != nil {
			return err
		}
		if proposal.Approval.Scope != proposal.Call.Scope || proposal.Approval.RunID != proposal.Call.RunID ||
			proposal.Approval.ActionCallID != proposal.Call.ID || proposal.Call.ApprovalID != proposal.Approval.ID {
			return ErrInvalidScope
		}
	}
	return nil
}

func actionIdempotencyKey(scope Scope, runID, key string) string {
	return portfolioKey(scope, runID) + ":" + key
}

func cloneActionCall(value *ActionCall) *ActionCall {
	if value == nil {
		return nil
	}
	var result ActionCall
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneApprovalCheckpoint(value *ApprovalCheckpoint) *ApprovalCheckpoint {
	if value == nil {
		return nil
	}
	var result ApprovalCheckpoint
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func containsActionStatus(values []ActionCallStatus, status ActionCallStatus) bool {
	for _, value := range values {
		if value == status {
			return true
		}
	}
	return false
}

func containsApprovalStatus(values []ApprovalStatus, status ApprovalStatus) bool {
	for _, value := range values {
		if value == status {
			return true
		}
	}
	return false
}

func pageActionCalls(values []*ActionCall, offset, limit int) []*ActionCall {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*ActionCall{}
	}
	values = values[offset:]
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(values) > limit {
		values = values[:limit]
	}
	return values
}

func pageApprovals(values []*ApprovalCheckpoint, offset, limit int) []*ApprovalCheckpoint {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*ApprovalCheckpoint{}
	}
	values = values[offset:]
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(values) > limit {
		values = values[:limit]
	}
	return values
}
