package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"
)

func (s *MemoryStore) CreateExternalConversationEndpoint(_ context.Context, endpoint *ExternalConversationEndpoint) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := externalConversationEndpointKey(endpoint.Scope, endpoint.ID)
	if _, exists := s.externalEndpoints[key]; exists {
		return ErrExternalConversationConflict
	}
	for _, existing := range s.externalEndpoints {
		if existing.IngressRoute == endpoint.IngressRoute {
			return ErrExternalConversationConflict
		}
	}
	s.externalEndpoints[key] = cloneExternalConversationEndpoint(endpoint)
	return nil
}

func (s *MemoryStore) GetExternalConversationEndpointByIngressRoute(_ context.Context, route string) (*ExternalConversationEndpoint, error) {
	route = strings.TrimSpace(route)
	if !validOpaqueIdentifier(route, 128) {
		return nil, ErrInvalidExternalConversation
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, endpoint := range s.externalEndpoints {
		if endpoint.IngressRoute == route {
			return cloneExternalConversationEndpoint(endpoint), nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) GetExternalConversationEndpoint(_ context.Context, scope Scope, id string) (*ExternalConversationEndpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneExternalConversationEndpoint(s.externalEndpoints[externalConversationEndpointKey(scope, strings.TrimSpace(id))]), nil
}

func (s *MemoryStore) ListExternalConversationEndpoints(_ context.Context, filter ExternalConversationEndpointFilter) ([]*ExternalConversationEndpoint, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*ExternalConversationEndpoint, 0)
	for _, endpoint := range s.externalEndpoints {
		if endpoint.Scope != filter.Scope || (filter.Owner != nil && endpoint.Owner != *filter.Owner) ||
			!externalConversationEndpointStatusMatches(endpoint.Status, filter.Statuses) {
			continue
		}
		if filter.Provider != "" && endpoint.Provider != filter.Provider {
			continue
		}
		items = append(items, cloneExternalConversationEndpoint(endpoint))
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		return items[i].ID < items[j].ID
	})
	return paginateExternalConversationEndpoints(items, filter.Limit, filter.Offset), nil
}

func (s *MemoryStore) ListExternalConversationEndpointsByVerifiedRoute(
	_ context.Context,
	route ExternalConversationVerifiedRoute,
) ([]*ExternalConversationEndpoint, error) {
	if err := route.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*ExternalConversationEndpoint, 0)
	for _, endpoint := range s.externalEndpoints {
		if externalConversationEndpointMatchesVerifiedRoute(endpoint, route) {
			items = append(items, cloneExternalConversationEndpoint(endpoint))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Scope.key() != items[j].Scope.key() {
			return items[i].Scope.key() < items[j].Scope.key()
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func externalConversationEndpointMatchesVerifiedRoute(
	endpoint *ExternalConversationEndpoint,
	route ExternalConversationVerifiedRoute,
) bool {
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Provider != route.Provider || endpoint.InstallationID != route.InstallationID ||
		endpoint.Address != route.Address || endpoint.Adapter.SkillID != route.SkillID ||
		endpoint.Adapter.SkillVersion != route.SkillVersion ||
		endpoint.Adapter.SourceIdentity != route.SourceIdentity ||
		endpoint.Adapter.AdapterID != route.AdapterID {
		return false
	}
	return route.ApplicationID == "" || endpoint.ApplicationID == route.ApplicationID
}

func (s *MemoryStore) UpdateExternalConversationEndpoint(_ context.Context, endpoint *ExternalConversationEndpoint, expectedRevision int64) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := externalConversationEndpointKey(endpoint.Scope, endpoint.ID)
	current := s.externalEndpoints[key]
	if current == nil {
		return ErrExternalConversationEndpointNotFound
	}
	if current.Revision != expectedRevision || endpoint.Revision != expectedRevision+1 ||
		current.Scope != endpoint.Scope || current.Owner != endpoint.Owner || current.DeploymentID != endpoint.DeploymentID ||
		current.Provider != endpoint.Provider || current.Mode != endpoint.Mode ||
		current.IngressRoute != endpoint.IngressRoute ||
		!current.CreatedAt.Equal(endpoint.CreatedAt) {
		return ErrExternalConversationConflict
	}
	s.externalEndpoints[key] = cloneExternalConversationEndpoint(endpoint)
	return nil
}

func (s *MemoryStore) ReceiveExternalConversationEvent(_ context.Context, item *ExternalConversationInboxItem) (*ExternalConversationInboxItem, bool, error) {
	if err := item.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	endpoint := s.externalEndpoints[externalConversationEndpointKey(item.Scope, item.EndpointID)]
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive || endpoint.Revision != item.EndpointRevision || endpoint.Adapter != item.Adapter {
		return nil, false, ErrExternalConversationConflict
	}
	deduplicationKey := externalConversationInboxDeduplicationKey(item.Scope, item.EndpointID, item.Event.ID)
	if existingID := s.externalInboxKeys[deduplicationKey]; existingID != "" {
		existing := s.externalInbox[externalConversationInboxKey(item.Scope, existingID)]
		if !sameExternalConversationInboxIntent(existing, item) {
			return nil, false, ErrExternalConversationConflict
		}
		return cloneExternalConversationInboxItem(existing), true, nil
	}
	key := externalConversationInboxKey(item.Scope, item.ID)
	if _, exists := s.externalInbox[key]; exists {
		return nil, false, ErrExternalConversationConflict
	}
	s.externalInbox[key] = cloneExternalConversationInboxItem(item)
	s.externalInboxKeys[deduplicationKey] = item.ID
	return cloneExternalConversationInboxItem(item), false, nil
}

func (s *MemoryStore) GetExternalConversationInboxItem(_ context.Context, scope Scope, id string) (*ExternalConversationInboxItem, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneExternalConversationInboxItem(s.externalInbox[externalConversationInboxKey(scope, strings.TrimSpace(id))]), nil
}

func (s *MemoryStore) ListExternalConversationInbox(_ context.Context, filter ExternalConversationInboxFilter) ([]*ExternalConversationInboxItem, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*ExternalConversationInboxItem, 0)
	for _, item := range s.externalInbox {
		if item.Scope != filter.Scope || (filter.EndpointID != "" && item.EndpointID != filter.EndpointID) ||
			!externalConversationInboxStatusMatches(item.Status, filter.Statuses) {
			continue
		}
		items = append(items, cloneExternalConversationInboxItem(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	return paginateExternalConversationInbox(items, filter.Limit, filter.Offset), nil
}

func (s *MemoryStore) ClaimExternalConversationInbox(_ context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*ExternalConversationInboxItem, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidExternalConversation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected *ExternalConversationInboxItem
	for _, item := range s.externalInbox {
		if item.Scope != scope || item.AvailableAt.After(now) || !externalConversationInboxClaimable(item, now) {
			continue
		}
		if externalConversationInboxOrderingLeased(s.externalInbox, item, now) {
			continue
		}
		endpoint := s.externalEndpoints[externalConversationEndpointKey(scope, item.EndpointID)]
		if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive || endpoint.Revision != item.EndpointRevision {
			continue
		}
		if selected == nil || item.AvailableAt.Before(selected.AvailableAt) ||
			(item.AvailableAt.Equal(selected.AvailableAt) && item.CreatedAt.Before(selected.CreatedAt)) ||
			(item.AvailableAt.Equal(selected.AvailableAt) && item.CreatedAt.Equal(selected.CreatedAt) && item.ID < selected.ID) {
			selected = item
		}
	}
	if selected == nil {
		return nil, nil
	}
	selected.Status = ExternalConversationInboxLeased
	selected.Attempt++
	selected.LeaseOwner = strings.TrimSpace(worker)
	selected.LeaseExpiresAt = now.Add(leaseDuration).UTC()
	selected.Revision++
	selected.UpdatedAt = now.UTC()
	return cloneExternalConversationInboxItem(selected), nil
}

func (s *MemoryStore) SaveExternalConversationInbox(_ context.Context, item *ExternalConversationInboxItem, expectedRevision int64, leaseOwner string) error {
	if err := item.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.externalInbox[externalConversationInboxKey(item.Scope, item.ID)]
	if current == nil {
		return ErrExternalConversationInboxNotFound
	}
	if current.Revision != expectedRevision || item.Revision != expectedRevision+1 ||
		current.Status != ExternalConversationInboxLeased || current.LeaseOwner != strings.TrimSpace(leaseOwner) ||
		item.UpdatedAt.Before(current.UpdatedAt) || item.UpdatedAt.After(current.LeaseExpiresAt) ||
		current.EndpointID != item.EndpointID || current.EndpointRevision != item.EndpointRevision ||
		current.Adapter != item.Adapter || !reflect.DeepEqual(current.Event, item.Event) ||
		!current.CreatedAt.Equal(item.CreatedAt) {
		return ErrExternalConversationLeaseLost
	}
	s.externalInbox[externalConversationInboxKey(item.Scope, item.ID)] = cloneExternalConversationInboxItem(item)
	return nil
}

func (s *MemoryStore) GetExternalConversationMapping(_ context.Context, scope Scope, endpointID, externalConversationID, externalThreadID string) (*ExternalConversationMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneExternalConversationMapping(s.externalMappings[externalConversationMappingKey(scope, endpointID, externalConversationID, externalThreadID)]), nil
}

func (s *MemoryStore) SaveExternalConversationMapping(_ context.Context, mapping *ExternalConversationMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := externalConversationMappingKey(mapping.Scope, mapping.EndpointID, mapping.ExternalConversationID, mapping.ExternalThreadID)
	current := s.externalMappings[key]
	if !validExternalMappingRevision(current == nil, currentRevisionExternalConversation(current), mapping.Revision, expectedRevision) {
		return ErrExternalConversationConflict
	}
	if current != nil && (current.Scope != mapping.Scope || current.EndpointID != mapping.EndpointID ||
		current.ExternalConversationID != mapping.ExternalConversationID || current.ExternalThreadID != mapping.ExternalThreadID ||
		current.ConversationID != mapping.ConversationID ||
		!current.CreatedAt.Equal(mapping.CreatedAt)) {
		return ErrExternalConversationConflict
	}
	s.externalMappings[key] = cloneExternalConversationMapping(mapping)
	return nil
}

func (s *MemoryStore) GetExternalParticipantMapping(_ context.Context, scope Scope, endpointID, externalParticipantID string) (*ExternalParticipantMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneExternalParticipantMapping(s.externalParticipants[externalParticipantMappingKey(scope, endpointID, externalParticipantID)]), nil
}

func (s *MemoryStore) SaveExternalParticipantMapping(_ context.Context, mapping *ExternalParticipantMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := externalParticipantMappingKey(mapping.Scope, mapping.EndpointID, mapping.ExternalParticipantID)
	current := s.externalParticipants[key]
	if !validExternalMappingRevision(current == nil, currentRevisionExternalParticipant(current), mapping.Revision, expectedRevision) {
		return ErrExternalConversationConflict
	}
	if current != nil && (current.Scope != mapping.Scope || current.EndpointID != mapping.EndpointID ||
		current.ExternalParticipantID != mapping.ExternalParticipantID || current.Participant != mapping.Participant ||
		!current.CreatedAt.Equal(mapping.CreatedAt)) {
		return ErrExternalConversationConflict
	}
	s.externalParticipants[key] = cloneExternalParticipantMapping(mapping)
	return nil
}

func (s *MemoryStore) GetExternalMessageMapping(_ context.Context, scope Scope, endpointID string, direction ExternalMessageDirection, externalMessageID string) (*ExternalMessageMapping, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneExternalMessageMapping(s.externalMessages[externalMessageMappingKey(scope, endpointID, direction, externalMessageID)]), nil
}

func (s *MemoryStore) SaveExternalMessageMapping(_ context.Context, mapping *ExternalMessageMapping, expectedRevision int64) error {
	if err := mapping.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := externalMessageMappingKey(mapping.Scope, mapping.EndpointID, mapping.Direction, mapping.ExternalMessageID)
	current := s.externalMessages[key]
	if !validExternalMappingRevision(current == nil, currentRevisionExternalMessage(current), mapping.Revision, expectedRevision) {
		return ErrExternalConversationConflict
	}
	if current != nil && (current.Scope != mapping.Scope || current.EndpointID != mapping.EndpointID ||
		current.Direction != mapping.Direction || current.ExternalMessageID != mapping.ExternalMessageID ||
		current.ConversationID != mapping.ConversationID || current.ChannelMessageID != mapping.ChannelMessageID ||
		!current.CreatedAt.Equal(mapping.CreatedAt)) {
		return ErrExternalConversationConflict
	}
	s.externalMessages[key] = cloneExternalMessageMapping(mapping)
	return nil
}

func (s *MemoryStore) EnqueueExternalConversationDelivery(_ context.Context, delivery *ExternalConversationDelivery) (*ExternalConversationDelivery, bool, error) {
	if err := delivery.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	endpoint := s.externalEndpoints[externalConversationEndpointKey(delivery.Scope, delivery.EndpointID)]
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive || endpoint.Revision != delivery.EndpointRevision ||
		endpoint.Adapter != delivery.Adapter {
		return nil, false, ErrExternalConversationConflict
	}
	deduplicationKey := externalConversationDeliveryDeduplicationKey(delivery.Scope, delivery.EndpointID, delivery.IdempotencyKey)
	if existingID := s.externalDeliveryKeys[deduplicationKey]; existingID != "" {
		existing := s.externalDeliveries[externalConversationDeliveryKey(delivery.Scope, existingID)]
		if sameExternalConversationDeliveryIntent(existing, delivery) {
			return cloneExternalConversationDelivery(existing), true, nil
		}
		rebound, ok := rebindExternalConversationDelivery(existing, delivery)
		if !ok {
			return nil, false, ErrExternalConversationConflict
		}
		s.externalDeliveries[externalConversationDeliveryKey(delivery.Scope, existingID)] = rebound
		return cloneExternalConversationDelivery(rebound), true, nil
	}
	key := externalConversationDeliveryKey(delivery.Scope, delivery.ID)
	if _, exists := s.externalDeliveries[key]; exists {
		return nil, false, ErrExternalConversationConflict
	}
	s.externalDeliveries[key] = cloneExternalConversationDelivery(delivery)
	s.externalDeliveryKeys[deduplicationKey] = delivery.ID
	return cloneExternalConversationDelivery(delivery), false, nil
}

func (s *MemoryStore) GetExternalConversationDelivery(_ context.Context, scope Scope, id string) (*ExternalConversationDelivery, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneExternalConversationDelivery(s.externalDeliveries[externalConversationDeliveryKey(scope, strings.TrimSpace(id))]), nil
}

func (s *MemoryStore) ListExternalConversationDeliveries(_ context.Context, filter ExternalConversationDeliveryFilter) ([]*ExternalConversationDelivery, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*ExternalConversationDelivery, 0)
	for _, delivery := range s.externalDeliveries {
		if delivery.Scope != filter.Scope || (filter.EndpointID != "" && delivery.EndpointID != filter.EndpointID) ||
			(filter.ConversationID != "" && delivery.ConversationID != filter.ConversationID) ||
			!externalConversationDeliveryStatusMatches(delivery.Status, filter.Statuses) {
			continue
		}
		items = append(items, cloneExternalConversationDelivery(delivery))
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	return paginateExternalConversationDeliveries(items, filter.Limit, filter.Offset), nil
}

func (s *MemoryStore) ClaimExternalConversationDelivery(_ context.Context, scope Scope, worker string, now time.Time, leaseDuration time.Duration) (*ExternalConversationDelivery, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(worker), 256) || now.IsZero() || leaseDuration <= 0 {
		return nil, ErrInvalidExternalConversation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var selected *ExternalConversationDelivery
	for _, delivery := range s.externalDeliveries {
		if delivery.Scope != scope || delivery.AvailableAt.After(now) || !externalConversationDeliveryClaimable(delivery, now) {
			continue
		}
		if externalConversationDeliveryOrderingLeased(s.externalDeliveries, delivery, now) {
			continue
		}
		endpoint := s.externalEndpoints[externalConversationEndpointKey(scope, delivery.EndpointID)]
		if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive || endpoint.Revision != delivery.EndpointRevision {
			continue
		}
		if selected == nil || delivery.AvailableAt.Before(selected.AvailableAt) ||
			(delivery.AvailableAt.Equal(selected.AvailableAt) && delivery.CreatedAt.Before(selected.CreatedAt)) ||
			(delivery.AvailableAt.Equal(selected.AvailableAt) && delivery.CreatedAt.Equal(selected.CreatedAt) && delivery.ID < selected.ID) {
			selected = delivery
		}
	}
	if selected == nil {
		return nil, nil
	}
	selected.Status = ExternalConversationDeliveryLeased
	selected.Attempt++
	selected.LeaseOwner = strings.TrimSpace(worker)
	selected.LeaseExpiresAt = now.Add(leaseDuration).UTC()
	selected.Revision++
	selected.UpdatedAt = now.UTC()
	return cloneExternalConversationDelivery(selected), nil
}

func (s *MemoryStore) SaveExternalConversationDelivery(_ context.Context, delivery *ExternalConversationDelivery, expectedRevision int64, leaseOwner string) error {
	if err := delivery.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.externalDeliveries[externalConversationDeliveryKey(delivery.Scope, delivery.ID)]
	if current == nil {
		return ErrExternalConversationDeliveryNotFound
	}
	if current.Revision != expectedRevision || delivery.Revision != expectedRevision+1 ||
		current.Status != ExternalConversationDeliveryLeased || current.LeaseOwner != strings.TrimSpace(leaseOwner) ||
		delivery.UpdatedAt.Before(current.UpdatedAt) || delivery.UpdatedAt.After(current.LeaseExpiresAt) ||
		current.EndpointID != delivery.EndpointID || current.EndpointRevision != delivery.EndpointRevision ||
		current.Adapter != delivery.Adapter || current.Operation != delivery.Operation ||
		current.ConversationID != delivery.ConversationID || current.ChannelMessageID != delivery.ChannelMessageID ||
		current.OrderingKey != delivery.OrderingKey || current.IdempotencyKey != delivery.IdempotencyKey ||
		!current.CreatedAt.Equal(delivery.CreatedAt) {
		return ErrExternalConversationLeaseLost
	}
	s.externalDeliveries[externalConversationDeliveryKey(delivery.Scope, delivery.ID)] = cloneExternalConversationDelivery(delivery)
	return nil
}

func externalConversationEndpointKey(scope Scope, id string) string {
	return scope.key() + "\x00" + strings.TrimSpace(id)
}

func externalConversationInboxKey(scope Scope, id string) string {
	return scope.key() + "\x00" + strings.TrimSpace(id)
}

func externalConversationInboxDeduplicationKey(scope Scope, endpointID, eventID string) string {
	return scope.key() + "\x00" + strings.TrimSpace(endpointID) + "\x00" + strings.TrimSpace(eventID)
}

func externalConversationMappingKey(scope Scope, endpointID, externalConversationID, externalThreadID string) string {
	return scope.key() + "\x00" + strings.TrimSpace(endpointID) + "\x00" + externalConversationID + "\x00" + externalThreadID
}

func externalParticipantMappingKey(scope Scope, endpointID, externalParticipantID string) string {
	return scope.key() + "\x00" + strings.TrimSpace(endpointID) + "\x00" + externalParticipantID
}

func externalMessageMappingKey(scope Scope, endpointID string, direction ExternalMessageDirection, externalMessageID string) string {
	return scope.key() + "\x00" + strings.TrimSpace(endpointID) + "\x00" + string(direction) + "\x00" + externalMessageID
}

func externalConversationDeliveryKey(scope Scope, id string) string {
	return scope.key() + "\x00" + strings.TrimSpace(id)
}

func externalConversationDeliveryDeduplicationKey(scope Scope, endpointID, idempotencyKey string) string {
	return scope.key() + "\x00" + strings.TrimSpace(endpointID) + "\x00" + strings.TrimSpace(idempotencyKey)
}

func sameExternalConversationInboxIntent(existing, candidate *ExternalConversationInboxItem) bool {
	return existing != nil && candidate != nil && existing.Scope == candidate.Scope &&
		existing.EndpointID == candidate.EndpointID && existing.EndpointRevision == candidate.EndpointRevision &&
		existing.Adapter == candidate.Adapter && reflect.DeepEqual(existing.Event, candidate.Event)
}

func sameExternalConversationDeliveryIntent(existing, candidate *ExternalConversationDelivery) bool {
	return existing != nil && candidate != nil && existing.Scope == candidate.Scope &&
		existing.EndpointID == candidate.EndpointID && existing.EndpointRevision == candidate.EndpointRevision &&
		existing.Adapter == candidate.Adapter && existing.Operation == candidate.Operation &&
		existing.ConversationID == candidate.ConversationID && existing.ChannelMessageID == candidate.ChannelMessageID &&
		existing.ExternalThreadID == candidate.ExternalThreadID && existing.OrderingKey == candidate.OrderingKey &&
		sameExternalConversationJSON(existing.Parameters, candidate.Parameters) && existing.IdempotencyKey == candidate.IdempotencyKey
}

func rebindExternalConversationDelivery(existing, candidate *ExternalConversationDelivery) (*ExternalConversationDelivery, bool) {
	if existing == nil || candidate == nil || existing.Scope != candidate.Scope ||
		existing.EndpointID != candidate.EndpointID || existing.Operation != candidate.Operation ||
		existing.ConversationID != candidate.ConversationID || existing.ChannelMessageID != candidate.ChannelMessageID ||
		existing.ExternalThreadID != candidate.ExternalThreadID || existing.OrderingKey != candidate.OrderingKey ||
		!sameExternalConversationJSON(existing.Parameters, candidate.Parameters) || existing.IdempotencyKey != candidate.IdempotencyKey ||
		(existing.Status != ExternalConversationDeliveryPending && existing.Status != ExternalConversationDeliveryRetry &&
			existing.Status != ExternalConversationDeliveryFailed) ||
		existing.ProviderMessageID != "" || !existing.DeliveredAt.IsZero() || existing.LeaseOwner != "" || !existing.LeaseExpiresAt.IsZero() {
		return nil, false
	}
	rebound := cloneExternalConversationDelivery(existing)
	rebound.EndpointRevision = candidate.EndpointRevision
	rebound.Adapter = candidate.Adapter
	rebound.Status = ExternalConversationDeliveryPending
	rebound.AvailableAt = candidate.AvailableAt
	rebound.ErrorCode = ""
	rebound.Summary = ""
	rebound.Revision++
	rebound.UpdatedAt = candidate.UpdatedAt
	if rebound.Validate() != nil {
		return nil, false
	}
	return rebound, true
}

func sameExternalConversationJSON(left, right interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func externalConversationInboxOrderingLeased(
	items map[string]*ExternalConversationInboxItem,
	candidate *ExternalConversationInboxItem,
	now time.Time,
) bool {
	for _, item := range items {
		if item.ID != candidate.ID && item.Scope == candidate.Scope && item.EndpointID == candidate.EndpointID &&
			item.Event.OrderingKey == candidate.Event.OrderingKey && item.Status == ExternalConversationInboxLeased &&
			item.LeaseExpiresAt.After(now) {
			return true
		}
	}
	return false
}

func externalConversationDeliveryOrderingLeased(
	items map[string]*ExternalConversationDelivery,
	candidate *ExternalConversationDelivery,
	now time.Time,
) bool {
	for _, item := range items {
		if item.ID != candidate.ID && item.Scope == candidate.Scope && item.EndpointID == candidate.EndpointID &&
			item.OrderingKey == candidate.OrderingKey && item.Status == ExternalConversationDeliveryLeased &&
			item.LeaseExpiresAt.After(now) {
			return true
		}
	}
	return false
}

func externalConversationInboxClaimable(item *ExternalConversationInboxItem, now time.Time) bool {
	return item.Status == ExternalConversationInboxPending || item.Status == ExternalConversationInboxRetry ||
		(item.Status == ExternalConversationInboxLeased && !item.LeaseExpiresAt.After(now))
}

func externalConversationDeliveryClaimable(item *ExternalConversationDelivery, now time.Time) bool {
	return item.Status == ExternalConversationDeliveryPending || item.Status == ExternalConversationDeliveryRetry ||
		(item.Status == ExternalConversationDeliveryLeased && !item.LeaseExpiresAt.After(now))
}

func externalConversationEndpointStatusMatches(value ExternalConversationEndpointStatus, wanted []ExternalConversationEndpointStatus) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, candidate := range wanted {
		if value == candidate {
			return true
		}
	}
	return false
}

func externalConversationInboxStatusMatches(value ExternalConversationInboxStatus, wanted []ExternalConversationInboxStatus) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, candidate := range wanted {
		if value == candidate {
			return true
		}
	}
	return false
}

func externalConversationDeliveryStatusMatches(value ExternalConversationDeliveryStatus, wanted []ExternalConversationDeliveryStatus) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, candidate := range wanted {
		if value == candidate {
			return true
		}
	}
	return false
}

func validExternalMappingRevision(missing bool, currentRevision, nextRevision, expectedRevision int64) bool {
	if expectedRevision < 0 || nextRevision != expectedRevision+1 {
		return false
	}
	if missing {
		return expectedRevision == 0
	}
	return currentRevision == expectedRevision
}

func currentRevisionExternalConversation(value *ExternalConversationMapping) int64 {
	if value == nil {
		return 0
	}
	return value.Revision
}

func currentRevisionExternalParticipant(value *ExternalParticipantMapping) int64 {
	if value == nil {
		return 0
	}
	return value.Revision
}

func currentRevisionExternalMessage(value *ExternalMessageMapping) int64 {
	if value == nil {
		return 0
	}
	return value.Revision
}

func paginateExternalConversationEndpoints(values []*ExternalConversationEndpoint, limit, offset int) []*ExternalConversationEndpoint {
	if offset >= len(values) {
		return []*ExternalConversationEndpoint{}
	}
	if offset < 0 {
		offset = 0
	}
	end := len(values)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return values[offset:end]
}

func paginateExternalConversationInbox(values []*ExternalConversationInboxItem, limit, offset int) []*ExternalConversationInboxItem {
	if offset >= len(values) {
		return []*ExternalConversationInboxItem{}
	}
	if offset < 0 {
		offset = 0
	}
	end := len(values)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return values[offset:end]
}

func paginateExternalConversationDeliveries(values []*ExternalConversationDelivery, limit, offset int) []*ExternalConversationDelivery {
	if offset >= len(values) {
		return []*ExternalConversationDelivery{}
	}
	if offset < 0 {
		offset = 0
	}
	end := len(values)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return values[offset:end]
}
