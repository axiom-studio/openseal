package runtime

import (
	"context"
	"sort"
)

func eventSourceSubscriptionKey(scope Scope, id string) string { return portfolioKey(scope, id) }

func (s *MemoryStore) CreateEventSourceSubscription(_ context.Context, subscription *EventSourceSubscription) error {
	if subscription == nil || subscription.Validate() != nil {
		return ErrInvalidEventSourceSubscription
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := eventSourceSubscriptionKey(subscription.Scope, subscription.ID)
	if _, exists := s.eventSourceSubscriptions[key]; exists {
		return ErrEventSourceSubscriptionConflict
	}
	s.eventSourceSubscriptions[key] = cloneEventSourceSubscription(subscription)
	return nil
}

func (s *MemoryStore) GetEventSourceSubscription(_ context.Context, scope Scope, id string) (*EventSourceSubscription, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(id, 512) {
		return nil, ErrInvalidEventSourceSubscription
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.eventSourceSubscriptions[eventSourceSubscriptionKey(scope, id)]
	if value == nil {
		return nil, ErrEventSourceSubscriptionNotFound
	}
	return cloneEventSourceSubscription(value), nil
}

func (s *MemoryStore) ListEventSourceSubscriptions(_ context.Context, filter EventSourceSubscriptionFilter) ([]*EventSourceSubscription, error) {
	if filter.Scope.Validate() != nil {
		return nil, ErrInvalidEventSourceSubscription
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*EventSourceSubscription, 0)
	for _, item := range s.eventSourceSubscriptions {
		if !matchesEventSourceSubscriptionFilter(item, filter) {
			continue
		}
		items = append(items, cloneEventSourceSubscription(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	start := filter.Offset
	if start > len(items) {
		start = len(items)
	}
	end := len(items)
	if filter.Limit > 0 && start+filter.Limit < end {
		end = start + filter.Limit
	}
	return items[start:end], nil
}

func (s *MemoryStore) UpdateEventSourceSubscription(_ context.Context, subscription *EventSourceSubscription, expectedRevision int64) error {
	if subscription == nil || subscription.Validate() != nil || subscription.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceSubscription
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := eventSourceSubscriptionKey(subscription.Scope, subscription.ID)
	current := s.eventSourceSubscriptions[key]
	if current == nil {
		return ErrEventSourceSubscriptionNotFound
	}
	if current.Revision != expectedRevision {
		return ErrEventSourceSubscriptionConflict
	}
	s.eventSourceSubscriptions[key] = cloneEventSourceSubscription(subscription)
	return nil
}

func (s *MemoryStore) GetEventSourceHealth(_ context.Context, scope Scope, subscriptionID string) (*EventSourceHealth, error) {
	if scope.Validate() != nil || !validOpaqueIdentifier(subscriptionID, 512) {
		return nil, ErrInvalidEventSourceHealth
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneEventSourceHealth(s.eventSourceHealth[eventSourceSubscriptionKey(scope, subscriptionID)]), nil
}

func (s *MemoryStore) SaveEventSourceHealth(_ context.Context, health *EventSourceHealth, expectedRevision int64) error {
	if health == nil || health.Validate() != nil || health.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceHealth
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := eventSourceSubscriptionKey(health.Scope, health.SubscriptionID)
	current := s.eventSourceHealth[key]
	currentRevision := int64(0)
	if current != nil {
		currentRevision = current.Revision
	}
	if currentRevision != expectedRevision {
		return ErrEventSourceSubscriptionConflict
	}
	s.eventSourceHealth[key] = cloneEventSourceHealth(health)
	return nil
}

func matchesEventSourceSubscriptionFilter(item *EventSourceSubscription, filter EventSourceSubscriptionFilter) bool {
	if item == nil || item.Scope != filter.Scope {
		return false
	}
	if filter.Owner != nil && item.Owner != *filter.Owner {
		return false
	}
	if filter.ConnectorKind != "" && item.Connector.Kind != filter.ConnectorKind {
		return false
	}
	if len(filter.Statuses) > 0 {
		matched := false
		for _, status := range filter.Statuses {
			if item.Status == status {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

var _ EventSourceSubscriptionStore = (*MemoryStore)(nil)
