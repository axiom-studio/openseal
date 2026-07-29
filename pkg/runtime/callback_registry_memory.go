package runtime

import (
	"context"
	"sort"
	"strings"
)

func callbackRegistrationKey(scope Scope, id string) string {
	return scope.Kind + ":" + scope.ID + ":" + strings.TrimSpace(id)
}

func (s *MemoryStore) CreateCallbackRegistration(_ context.Context, value *CallbackRegistration) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := callbackRegistrationKey(value.Scope, value.ID)
	if s.callbackRegistrations[key] != nil {
		return ErrCallbackRegistrationConflict
	}
	for _, current := range s.callbackRegistrations {
		if current.IngressRoute == value.IngressRoute {
			return ErrCallbackRegistrationConflict
		}
	}
	s.callbackRegistrations[key] = cloneCallbackRegistration(value)
	return nil
}

func (s *MemoryStore) GetCallbackRegistration(_ context.Context, scope Scope, id string) (*CallbackRegistration, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCallbackRegistration(s.callbackRegistrations[callbackRegistrationKey(scope, id)]), nil
}

func (s *MemoryStore) GetCallbackRegistrationByIngressRoute(_ context.Context, route string) (*CallbackRegistration, error) {
	if !validOpaqueIdentifier(strings.TrimSpace(route), 128) {
		return nil, ErrInvalidCallbackRegistration
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, value := range s.callbackRegistrations {
		if value.IngressRoute == strings.TrimSpace(route) {
			return cloneCallbackRegistration(value), nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) ListCallbackRegistrations(_ context.Context, filter CallbackRegistrationFilter) ([]*CallbackRegistration, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	status := make(map[CallbackRegistrationStatus]bool, len(filter.Statuses))
	for _, value := range filter.Statuses {
		status[value] = true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]*CallbackRegistration, 0)
	for _, value := range s.callbackRegistrations {
		if value.Scope != filter.Scope || (filter.Provider != "" && value.Provider != filter.Provider) ||
			(len(status) > 0 && !status[value.Status]) {
			continue
		}
		values = append(values, cloneCallbackRegistration(value))
	}
	sort.Slice(values, func(i, j int) bool {
		if !values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].UpdatedAt.After(values[j].UpdatedAt)
		}
		return values[i].ID < values[j].ID
	})
	limit, offset := normalizeExternalConversationPage(filter.Limit, filter.Offset)
	if offset >= len(values) {
		return []*CallbackRegistration{}, nil
	}
	end := offset + limit
	if end > len(values) {
		end = len(values)
	}
	return values[offset:end], nil
}

func (s *MemoryStore) UpdateCallbackRegistration(_ context.Context, value *CallbackRegistration, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := callbackRegistrationKey(value.Scope, value.ID)
	current := s.callbackRegistrations[key]
	if current == nil {
		return ErrCallbackRegistrationNotFound
	}
	if current.Revision != expectedRevision {
		return ErrCallbackRegistrationConflict
	}
	for candidateKey, candidate := range s.callbackRegistrations {
		if candidateKey != key && candidate.IngressRoute == value.IngressRoute {
			return ErrCallbackRegistrationConflict
		}
	}
	s.callbackRegistrations[key] = cloneCallbackRegistration(value)
	return nil
}
