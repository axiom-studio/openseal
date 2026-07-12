package runtime

import (
	"context"
	"sort"
)

func sourceObservationKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}

func sourceObservationDedupeStoreKey(observation *SourceObservation) string {
	return observation.Scope.Kind + "\x00" + observation.Scope.ID + "\x00" + observation.InitiativeID + "\x00" + observation.MonitorID + "\x00" + observation.DedupeKey
}

func sourceMonitorCheckpointKey(scope Scope, initiativeID, monitorID string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + initiativeID + "\x00" + monitorID
}

func (s *MemoryStore) IngestSourceObservation(_ context.Context, observation *SourceObservation, checkpoint *SourceMonitorCheckpoint, expected int64, event *ActivityEvent) (*SourceObservation, *SourceMonitorCheckpoint, *ActivityEvent, bool, error) {
	if err := observation.Validate(); err != nil {
		return nil, nil, nil, false, err
	}
	if event == nil || event.Validate() != nil {
		return nil, nil, nil, false, ErrInvalidSourceObservation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpointKey := sourceMonitorCheckpointKey(observation.Scope, observation.InitiativeID, observation.MonitorID)
	current := s.sourceMonitorCheckpoints[checkpointKey]
	currentRevision := int64(0)
	if current != nil {
		currentRevision = current.Revision
	}
	dedupeKey := sourceObservationDedupeStoreKey(observation)
	existingID := s.sourceObservationKeys[dedupeKey]
	existing := s.sourceObservations[sourceObservationKey(observation.Scope, existingID)]
	if currentRevision != expected {
		if existing != nil && current != nil && current.LastObservationID == existing.ID && current.Cursor == checkpoint.Cursor {
			return cloneSourceObservation(existing), cloneSourceMonitorCheckpoint(current), nil, true, nil
		}
		return nil, nil, nil, false, ErrSourceMonitorCheckpoint
	}
	next := cloneSourceMonitorCheckpoint(checkpoint)
	next.Revision = expected + 1
	if existing != nil {
		next.ObservationCount = currentObservationCount(current)
		s.sourceMonitorCheckpoints[checkpointKey] = next
		return cloneSourceObservation(existing), cloneSourceMonitorCheckpoint(next), nil, true, nil
	}
	next.ObservationCount = currentObservationCount(current) + 1
	s.sourceObservations[sourceObservationKey(observation.Scope, observation.ID)] = cloneSourceObservation(observation)
	s.sourceObservationKeys[dedupeKey] = observation.ID
	s.sourceMonitorCheckpoints[checkpointKey] = next
	persisted := appendMemoryActivityLocked(s, event)
	return cloneSourceObservation(observation), cloneSourceMonitorCheckpoint(next), cloneActivityEvent(persisted), false, nil
}

func currentObservationCount(checkpoint *SourceMonitorCheckpoint) int64 {
	if checkpoint == nil {
		return 0
	}
	return checkpoint.ObservationCount
}

func (s *MemoryStore) GetSourceObservation(_ context.Context, scope Scope, id string) (*SourceObservation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.sourceObservations[sourceObservationKey(scope, id)]
	if value == nil {
		return nil, ErrSourceObservationNotFound
	}
	return cloneSourceObservation(value), nil
}

func (s *MemoryStore) ListSourceObservations(_ context.Context, filter SourceObservationFilter) ([]*SourceObservation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]*SourceObservation, 0)
	for _, value := range s.sourceObservations {
		if value.Scope != filter.Scope || filter.InitiativeID != "" && value.InitiativeID != filter.InitiativeID || filter.MonitorID != "" && value.MonitorID != filter.MonitorID || filter.RunID != "" && value.RunID != filter.RunID {
			continue
		}
		values = append(values, cloneSourceObservation(value))
	}
	sort.Slice(values, func(i, j int) bool { return values[i].IngestedAt.After(values[j].IngestedAt) })
	start := filter.Offset
	if start > len(values) {
		start = len(values)
	}
	end := len(values)
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if start+limit < end {
		end = start + limit
	}
	return values[start:end], nil
}

func (s *MemoryStore) GetSourceMonitorCheckpoint(_ context.Context, scope Scope, initiativeID, monitorID string) (*SourceMonitorCheckpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value := s.sourceMonitorCheckpoints[sourceMonitorCheckpointKey(scope, initiativeID, monitorID)]
	if value == nil {
		return nil, ErrSourceObservationNotFound
	}
	return cloneSourceMonitorCheckpoint(value), nil
}

var _ SourceMonitorStore = (*MemoryStore)(nil)
