package runtime

import "context"

func eventSourceCheckpointKey(scope Scope, source, subscriptionID string) string {
	return portfolioKey(scope, source+"\x00"+subscriptionID)
}

func (s *MemoryStore) GetEventSourceCheckpoint(_ context.Context, scope Scope, source, subscriptionID string) (*EventSourceCheckpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneEventSourceCheckpoint(s.eventSourceCheckpoints[eventSourceCheckpointKey(scope, source, subscriptionID)]), nil
}

func (s *MemoryStore) SaveEventSourceCheckpoint(_ context.Context, checkpoint *EventSourceCheckpoint, expectedRevision int64) error {
	if checkpoint == nil || checkpoint.Validate() != nil || checkpoint.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceCheckpoint
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := eventSourceCheckpointKey(checkpoint.Scope, checkpoint.Source, checkpoint.SubscriptionID)
	current := s.eventSourceCheckpoints[key]
	if (current == nil && expectedRevision != 0) || (current != nil && current.Revision != expectedRevision) {
		return ErrEventSourceCheckpointConflict
	}
	s.eventSourceCheckpoints[key] = cloneEventSourceCheckpoint(checkpoint)
	return nil
}

var _ EventSourceCheckpointStore = (*MemoryStore)(nil)
