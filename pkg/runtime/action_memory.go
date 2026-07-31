package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
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
	if call.ExternalOperationDigest != "" && call.DuplicateOfActionCallID == "" && externalOperationProtects(call.Status) {
		if existingID := s.externalOperationKeys[externalOperationStoreKey(call.Scope, call.ExternalOperationDigest)]; existingID != "" {
			existing := s.actions[portfolioKey(call.Scope, existingID)]
			if existing != nil && externalOperationProtects(existing.Status) {
				return nil, &ExternalOperationConflictError{Prior: cloneActionCall(existing)}
			}
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
	if call.ExternalOperationDigest != "" && call.DuplicateOfActionCallID == "" && externalOperationProtects(call.Status) {
		s.externalOperationKeys[externalOperationStoreKey(call.Scope, call.ExternalOperationDigest)] = call.ID
	}
	if proposal.Approval != nil {
		s.approvals[portfolioKey(call.Scope, proposal.Approval.ID)] = cloneApprovalCheckpoint(proposal.Approval)
	}
	s.agentRuns[runKey] = cloneAgentRun(proposal.Run)
	s.activity[runKey] = append(s.activity[runKey], event)
	return &ActionProposalResult{
		Call: cloneActionCall(call), Approval: cloneApprovalCheckpoint(proposal.Approval),
		Run: cloneAgentRun(proposal.Run), Event: cloneActivityEvent(event), Created: true,
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

func (s *MemoryStore) GetActionCallByExternalOperation(_ context.Context, scope Scope, digest string) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := validateSHA256Digest(digest); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.externalOperationKeys[externalOperationStoreKey(scope, digest)]
	call := s.actions[portfolioKey(scope, id)]
	if call == nil || !externalOperationProtects(call.Status) {
		return nil, ErrActionNotFound
	}
	return cloneActionCall(call), nil
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
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ApprovalCheckpoint, 0)
	for _, approval := range s.approvals {
		if approval.Scope != filter.Scope || filter.RunID != "" && approval.RunID != filter.RunID ||
			len(filter.Status) > 0 && !containsApprovalStatus(filter.Status, approval.Status) {
			continue
		}
		if filter.Owner != nil {
			run := s.agentRuns[portfolioKey(filter.Scope, approval.RunID)]
			if run == nil || run.Owner != *filter.Owner {
				continue
			}
		}
		result = append(result, cloneApprovalCheckpoint(approval))
	}
	sort.Slice(result, func(i, j int) bool {
		if filter.NewestFirst {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return pageApprovals(result, filter.Offset, filter.Limit), nil
}

func (s *MemoryStore) ResolveApproval(_ context.Context, resolution ApprovalResolutionRecord) (*ApprovalResolutionResult, error) {
	if err := validateApprovalResolutionRecord(resolution); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	approvalKey := portfolioKey(resolution.Approval.Scope, resolution.Approval.ID)
	currentApproval := s.approvals[approvalKey]
	if currentApproval == nil {
		return nil, ErrApprovalNotFound
	}
	callKey := portfolioKey(resolution.Call.Scope, resolution.Call.ID)
	runKey := portfolioKey(resolution.Run.Scope, resolution.Run.ID)
	currentCall := s.actions[callKey]
	currentRun := s.agentRuns[runKey]
	if currentCall == nil {
		return nil, ErrActionNotFound
	}
	if currentRun == nil {
		return nil, ErrRunNotFound
	}
	if currentApproval.Status != ApprovalStatusPending {
		if currentApproval.DecisionID != "" && currentApproval.DecisionID == resolution.Approval.DecisionID {
			return &ApprovalResolutionResult{Approval: cloneApprovalCheckpoint(currentApproval), Call: cloneActionCall(currentCall), Run: cloneAgentRun(currentRun), Resolved: false}, nil
		}
		return nil, ErrApprovalResolved
	}
	if currentApproval.Revision != resolution.ExpectedApprovalRevision || currentCall.Revision != resolution.ExpectedCallRevision || currentRun.Revision != resolution.ExpectedRunRevision ||
		resolution.Approval.Revision != resolution.ExpectedApprovalRevision+1 || resolution.Call.Revision != resolution.ExpectedCallRevision+1 || resolution.Run.Revision != resolution.ExpectedRunRevision+1 {
		return nil, ErrRevisionConflict
	}
	event := cloneActivityEvent(resolution.Event)
	event.Sequence = int64(len(s.activity[runKey]) + 1)
	s.approvals[approvalKey] = cloneApprovalCheckpoint(resolution.Approval)
	s.actions[callKey] = cloneActionCall(resolution.Call)
	if currentCall.ExternalOperationDigest != "" && externalOperationProtects(currentCall.Status) && !externalOperationProtects(resolution.Call.Status) {
		delete(s.externalOperationKeys, externalOperationStoreKey(currentCall.Scope, currentCall.ExternalOperationDigest))
	}
	s.agentRuns[runKey] = cloneAgentRun(resolution.Run)
	s.activity[runKey] = append(s.activity[runKey], event)
	return &ApprovalResolutionResult{Approval: cloneApprovalCheckpoint(resolution.Approval), Call: cloneActionCall(resolution.Call), Run: cloneAgentRun(resolution.Run), Event: cloneActivityEvent(event), Resolved: true}, nil
}

func (s *MemoryStore) ClaimNextAction(_ context.Context, claim ActionClaim) (*ActionCall, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected *ActionCall
	for _, call := range s.actions {
		if call.Scope != claim.Scope || !actionEligible(call, claim.Now) {
			continue
		}
		if selected == nil || actionSchedulesBefore(call, selected) {
			selected = call
		}
	}
	if selected == nil {
		return nil, nil
	}
	updated := cloneActionCall(selected)
	expires := claim.Now.Add(claim.LeaseDuration)
	updated.Status = ActionCallStatusRunning
	updated.LeaseOwner = claim.WorkerID
	updated.LeaseExpiresAt = &expires
	updated.Attempt++
	updated.Revision++
	updated.UpdatedAt = claim.Now
	if updated.StartedAt == nil {
		updated.StartedAt = &claim.Now
	}
	s.actions[portfolioKey(updated.Scope, updated.ID)] = cloneActionCall(updated)
	return updated, nil
}

func (s *MemoryStore) RenewActionLease(_ context.Context, scope Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workerID) == "" || now.IsZero() || leaseDuration <= 0 {
		return nil, errors.New("worker, current time, and lease duration are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.actions[portfolioKey(scope, actionID)]
	if current == nil {
		return nil, ErrActionNotFound
	}
	if current.Status != ActionCallStatusRunning || current.LeaseOwner != workerID || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return nil, ErrLeaseLost
	}
	updated := cloneActionCall(current)
	expires := now.Add(leaseDuration)
	updated.LeaseExpiresAt = &expires
	updated.UpdatedAt = now
	updated.Revision++
	s.actions[portfolioKey(scope, actionID)] = cloneActionCall(updated)
	return updated, nil
}

func (s *MemoryStore) PersistActionExecution(_ context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error) {
	if err := validateActionExecutionRecord(execution); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	callKey := portfolioKey(execution.Call.Scope, execution.Call.ID)
	currentCall := s.actions[callKey]
	if currentCall == nil {
		return nil, ErrActionNotFound
	}
	if currentCall.Revision != execution.ExpectedCallRevision || execution.Call.Revision != execution.ExpectedCallRevision+1 {
		return nil, ErrRevisionConflict
	}
	if currentCall.Status != ActionCallStatusRunning || currentCall.LeaseOwner != execution.WorkerID || currentCall.LeaseExpiresAt == nil || !currentCall.LeaseExpiresAt.After(execution.Now) {
		return nil, ErrLeaseLost
	}
	runKey := portfolioKey(execution.Call.Scope, execution.Call.RunID)
	currentRun := s.agentRuns[runKey]
	if currentRun == nil {
		return nil, ErrRunNotFound
	}
	if execution.Run != nil && (currentRun.Revision != execution.ExpectedRunRevision || execution.Run.Revision != execution.ExpectedRunRevision+1) {
		return nil, ErrRevisionConflict
	}
	event := cloneActivityEvent(execution.Event)
	event.Sequence = int64(len(s.activity[runKey]) + 1)
	s.actions[callKey] = cloneActionCall(execution.Call)
	if currentCall.ExternalOperationDigest != "" && externalOperationProtects(currentCall.Status) && !externalOperationProtects(execution.Call.Status) {
		delete(s.externalOperationKeys, externalOperationStoreKey(currentCall.Scope, currentCall.ExternalOperationDigest))
	}
	if execution.Run != nil {
		s.agentRuns[runKey] = cloneAgentRun(execution.Run)
	}
	s.activity[runKey] = append(s.activity[runKey], event)
	return &ActionExecutionResult{Call: cloneActionCall(execution.Call), Run: cloneAgentRun(execution.Run), Event: cloneActivityEvent(event)}, nil
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

func validateApprovalResolutionRecord(resolution ApprovalResolutionRecord) error {
	if err := resolution.Approval.Validate(); err != nil {
		return err
	}
	if err := resolution.Call.Validate(); err != nil {
		return err
	}
	if err := resolution.Run.Validate(); err != nil {
		return err
	}
	if err := resolution.Event.Validate(); err != nil {
		return err
	}
	if resolution.Approval.Status == ApprovalStatusPending || strings.TrimSpace(resolution.Approval.DecisionID) == "" || resolution.Approval.DecisionBy == nil || resolution.Approval.DecidedAt == nil {
		return errors.New("approval resolution requires a terminal decision, id, principal, and decision time")
	}
	if resolution.Approval.Scope != resolution.Call.Scope || resolution.Approval.Scope != resolution.Run.Scope || resolution.Approval.Scope != resolution.Event.Scope ||
		resolution.Approval.RunID != resolution.Run.ID || resolution.Approval.RunID != resolution.Call.RunID || resolution.Approval.RunID != resolution.Event.RunID ||
		resolution.Approval.ActionCallID != resolution.Call.ID || resolution.Call.ApprovalID != resolution.Approval.ID {
		return ErrInvalidScope
	}
	return nil
}

func validateActionExecutionRecord(execution ActionExecutionRecord) error {
	if err := execution.Call.Validate(); err != nil {
		return err
	}
	if err := execution.Event.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(execution.WorkerID) == "" || execution.Now.IsZero() {
		return errors.New("action execution worker and current time are required")
	}
	if execution.Call.Status != ActionCallStatusReady && execution.Call.Status != ActionCallStatusSucceeded && execution.Call.Status != ActionCallStatusFailed && execution.Call.Status != ActionCallStatusCanceled {
		return errors.New("persisted action execution must retry or finish")
	}
	if execution.Call.Scope != execution.Event.Scope || execution.Call.RunID != execution.Event.RunID {
		return ErrInvalidScope
	}
	if execution.Run != nil {
		if err := execution.Run.Validate(); err != nil {
			return err
		}
		if execution.Run.Scope != execution.Call.Scope || execution.Run.ID != execution.Call.RunID || execution.Call.Status == ActionCallStatusReady {
			return ErrInvalidScope
		}
	}
	return nil
}

func actionEligible(call *ActionCall, now time.Time) bool {
	return (call.Status == ActionCallStatusReady && !call.AvailableAt.After(now)) ||
		(call.Status == ActionCallStatusRunning && (call.LeaseExpiresAt == nil || !call.LeaseExpiresAt.After(now)))
}

func actionSchedulesBefore(left, right *ActionCall) bool {
	if !left.AvailableAt.Equal(right.AvailableAt) {
		return left.AvailableAt.Before(right.AvailableAt)
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}

func actionIdempotencyKey(scope Scope, runID, key string) string {
	return portfolioKey(scope, runID) + ":" + key
}

func externalOperationStoreKey(scope Scope, digest string) string {
	return portfolioKey(scope, strings.TrimSpace(digest))
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
