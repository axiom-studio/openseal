package runtime

import (
	"context"
	"errors"
	"sort"
	"strings"
)

func requestStoreKey(scope Scope, id string) string { return scope.key() + ":" + id }

func requestIdempotencyStoreKey(scope Scope, key string) string { return scope.key() + ":" + key }

func (s *MemoryStore) CreateAgentRequest(_ context.Context, record AgentRequestCreateRecord) (*ActivityEvent, error) {
	if record.Request == nil || record.Event == nil {
		return nil, ErrAgentRequestNotFound
	}
	if err := record.Request.Validate(); err != nil {
		return nil, err
	}
	if err := record.Event.Validate(); err != nil {
		return nil, err
	}
	if record.Request.Scope != record.Event.Scope || record.Request.SourceRunID != record.Event.RunID {
		return nil, ErrInvalidScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runKey := portfolioKey(record.Request.Scope, record.Request.SourceRunID)
	if s.agentRuns[runKey] == nil {
		return nil, ErrRunNotFound
	}
	requestKey := requestStoreKey(record.Request.Scope, record.Request.ID)
	if s.requests[requestKey] != nil {
		return nil, ErrAgentRequestIdempotency
	}
	if record.Request.IdempotencyKey != "" {
		key := requestIdempotencyStoreKey(record.Request.Scope, record.Request.IdempotencyKey)
		if s.requestKeys[key] != "" {
			return nil, ErrAgentRequestIdempotency
		}
		s.requestKeys[key] = record.Request.ID
	}
	s.requests[requestKey] = cloneAgentRequest(record.Request)
	persisted := appendMemoryActivityLocked(s, record.Event)
	return cloneActivityEvent(persisted), nil
}

func (s *MemoryStore) GetAgentRequest(_ context.Context, scope Scope, requestID string) (*AgentRequest, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAgentRequest(s.requests[requestStoreKey(scope, requestID)]), nil
}

func (s *MemoryStore) FindAgentRequestByIdempotencyKey(_ context.Context, scope Scope, key string) (*AgentRequest, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.requestKeys[requestIdempotencyStoreKey(scope, strings.TrimSpace(key))]
	return cloneAgentRequest(s.requests[requestStoreKey(scope, id)]), nil
}

func (s *MemoryStore) ListAgentRequests(_ context.Context, filter AgentRequestFilter) ([]*AgentRequest, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*AgentRequest, 0)
	for _, request := range s.requests {
		if request.Scope == filter.Scope && matchesAgentRequestFilter(request, filter) {
			result = append(result, cloneAgentRequest(request))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return pageAgentRequests(result, filter.Offset, filter.Limit), nil
}

func (s *MemoryStore) RespondAgentRequest(_ context.Context, record AgentRequestResponseRecord) ([]*ActivityEvent, error) {
	if err := validateAgentRequestResponseRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := requestStoreKey(record.Request.Scope, record.Request.ID)
	current := s.requests[key]
	if current == nil {
		return nil, ErrAgentRequestNotFound
	}
	if current.Revision != record.ExpectedRequestRevision || record.Request.Revision != current.Revision+1 {
		return nil, ErrRevisionConflict
	}
	if record.SourceRun != nil {
		sourceKey := portfolioKey(record.SourceRun.Scope, record.SourceRun.ID)
		currentSource := s.agentRuns[sourceKey]
		if currentSource == nil {
			return nil, ErrRunNotFound
		}
		if currentSource.Revision != record.ExpectedSourceRevision || record.SourceRun.Revision != currentSource.Revision+1 {
			return nil, ErrRevisionConflict
		}
		if record.ChildRun == nil || s.agentRuns[portfolioKey(record.ChildRun.Scope, record.ChildRun.ID)] != nil {
			return nil, ErrInvalidAgentRequestState
		}
		s.agentRuns[sourceKey] = cloneAgentRun(record.SourceRun)
		s.agentRuns[portfolioKey(record.ChildRun.Scope, record.ChildRun.ID)] = cloneAgentRun(record.ChildRun)
	}
	s.requests[key] = cloneAgentRequest(record.Request)
	events := make([]*ActivityEvent, 0, 2)
	if record.SourceEvent != nil {
		events = append(events, cloneActivityEvent(appendMemoryActivityLocked(s, record.SourceEvent)))
	}
	if record.ChildEvent != nil {
		events = append(events, cloneActivityEvent(appendMemoryActivityLocked(s, record.ChildEvent)))
	}
	return events, nil
}

func (s *MemoryStore) CompleteAgentRequest(_ context.Context, record AgentRequestCompletionRecord) ([]*ActivityEvent, error) {
	if err := validateAgentRequestCompletionRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	requestKey := requestStoreKey(record.Request.Scope, record.Request.ID)
	currentRequest := s.requests[requestKey]
	if currentRequest == nil {
		return nil, ErrAgentRequestNotFound
	}
	if currentRequest.Revision != record.ExpectedRequestRevision || record.Request.Revision != currentRequest.Revision+1 {
		return nil, ErrRevisionConflict
	}
	sourceKey := portfolioKey(record.SourceRun.Scope, record.SourceRun.ID)
	childKey := portfolioKey(record.ChildRun.Scope, record.ChildRun.ID)
	currentSource := s.agentRuns[sourceKey]
	currentChild := s.agentRuns[childKey]
	if currentSource == nil || currentChild == nil {
		return nil, ErrRunNotFound
	}
	if currentSource.Revision != record.ExpectedSourceRevision || record.SourceRun.Revision != currentSource.Revision+1 ||
		currentChild.Revision != record.ExpectedChildRevision || record.ChildRun.Revision != currentChild.Revision+1 {
		return nil, ErrRevisionConflict
	}
	s.requests[requestKey] = cloneAgentRequest(record.Request)
	s.agentRuns[sourceKey] = cloneAgentRun(record.SourceRun)
	s.agentRuns[childKey] = cloneAgentRun(record.ChildRun)
	events := make([]*ActivityEvent, 0, 2)
	for _, event := range []*ActivityEvent{record.SourceEvent, record.ChildEvent} {
		events = append(events, cloneActivityEvent(appendMemoryActivityLocked(s, event)))
	}
	return events, nil
}

func appendMemoryActivityLocked(s *MemoryStore, event *ActivityEvent) *ActivityEvent {
	key := portfolioKey(event.Scope, event.RunID)
	persisted := cloneActivityEvent(event)
	persisted.Sequence = int64(len(s.activity[key]) + 1)
	s.activity[key] = append(s.activity[key], persisted)
	return persisted
}

func matchesAgentRequestFilter(request *AgentRequest, filter AgentRequestFilter) bool {
	if filter.SourceRunID != "" && request.SourceRunID != filter.SourceRunID {
		return false
	}
	if filter.Requester != nil && request.Requester != *filter.Requester {
		return false
	}
	if filter.Recipient != nil && request.Recipient != *filter.Recipient {
		return false
	}
	if len(filter.Kinds) > 0 && !containsAgentRequestKind(filter.Kinds, request.Kind) {
		return false
	}
	return len(filter.Statuses) == 0 || containsAgentRequestStatus(filter.Statuses, request.Status)
}

func containsAgentRequestKind(values []AgentRequestKind, value AgentRequestKind) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func containsAgentRequestStatus(values []AgentRequestStatus, value AgentRequestStatus) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func pageAgentRequests(values []*AgentRequest, offset, limit int) []*AgentRequest {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*AgentRequest{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}

func validateAgentRequestResponseRecord(record AgentRequestResponseRecord) error {
	if record.Request == nil || record.SourceEvent == nil {
		return errors.New("agent request response and source event are required")
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if err := record.SourceEvent.Validate(); err != nil {
		return err
	}
	if record.SourceRun != nil {
		if record.ChildRun == nil || record.ChildEvent == nil {
			return errors.New("accepted agent request requires source run, child run, and child event")
		}
		if err := record.SourceRun.Validate(); err != nil {
			return err
		}
		if err := record.ChildRun.Validate(); err != nil {
			return err
		}
		if err := record.ChildEvent.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateAgentRequestCompletionRecord(record AgentRequestCompletionRecord) error {
	if record.Request == nil || record.SourceRun == nil || record.ChildRun == nil || record.SourceEvent == nil || record.ChildEvent == nil {
		return errors.New("agent request completion requires request, source run, child run, and both events")
	}
	if err := record.Request.Validate(); err != nil {
		return err
	}
	if record.Request.Status != AgentRequestStatusCompleted {
		return ErrInvalidAgentRequestState
	}
	for _, run := range []*AgentRun{record.SourceRun, record.ChildRun} {
		if err := run.Validate(); err != nil {
			return err
		}
	}
	for _, event := range []*ActivityEvent{record.SourceEvent, record.ChildEvent} {
		if err := event.Validate(); err != nil {
			return err
		}
	}
	return nil
}
