package runtime

import (
	"context"
	"strings"
)

func externalConversationGatewayKey(scope Scope, id string) string {
	return scope.key() + "\x00" + strings.TrimSpace(id)
}

func (s *MemoryStore) CreateExternalConversationGateway(
	_ context.Context,
	value *ExternalConversationGatewayRegistration,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := externalConversationGatewayKey(value.Gateway.Scope, value.ID)
	if s.externalGateways[key] != nil {
		return ErrExternalConversationConflict
	}
	for _, existing := range s.externalGateways {
		if existing.IngressRoute == value.IngressRoute {
			return ErrExternalConversationConflict
		}
	}
	s.externalGateways[key] = cloneExternalConversationGateway(value)
	return nil
}

func (s *MemoryStore) GetExternalConversationGateway(
	_ context.Context,
	scope Scope,
	id string,
) (*ExternalConversationGatewayRegistration, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneExternalConversationGateway(s.externalGateways[externalConversationGatewayKey(scope, id)]), nil
}

func (s *MemoryStore) GetExternalConversationGatewayByIngressRoute(
	_ context.Context,
	route string,
) (*ExternalConversationGatewayRegistration, error) {
	if !validOpaqueIdentifier(strings.TrimSpace(route), 128) {
		return nil, ErrInvalidExternalConversation
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, value := range s.externalGateways {
		if value.IngressRoute == strings.TrimSpace(route) {
			return cloneExternalConversationGateway(value), nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) ListExternalConversationGateways(
	_ context.Context,
	filter ExternalConversationGatewayFilter,
) ([]*ExternalConversationGatewayRegistration, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]*ExternalConversationGatewayRegistration, 0)
	for _, value := range s.externalGateways {
		if value.Gateway.Scope != filter.Scope ||
			(filter.Provider != "" && value.Gateway.Provider != filter.Provider) ||
			!externalConversationGatewayStatusMatches(value.Status, filter.Statuses) {
			continue
		}
		values = append(values, cloneExternalConversationGateway(value))
	}
	sortExternalConversationGateways(values)
	return paginateExternalConversationGateways(values, filter.Limit, filter.Offset), nil
}

func (s *MemoryStore) UpdateExternalConversationGateway(
	_ context.Context,
	value *ExternalConversationGatewayRegistration,
	expectedRevision int64,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := externalConversationGatewayKey(value.Gateway.Scope, value.ID)
	current := s.externalGateways[key]
	if current == nil {
		return ErrExternalConversationGatewayNotFound
	}
	if current.Revision != expectedRevision {
		return ErrExternalConversationConflict
	}
	for candidateKey, existing := range s.externalGateways {
		if candidateKey != key && existing.IngressRoute == value.IngressRoute {
			return ErrExternalConversationConflict
		}
	}
	s.externalGateways[key] = cloneExternalConversationGateway(value)
	return nil
}
