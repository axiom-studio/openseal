package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type memoryRoutineEventKey struct {
	scope        Scope
	activationID string
	eventID      string
}

type memoryRoutineEventBatchKey struct {
	scope Scope
	id    string
}

func cloneRoutineEventRecord(record *RoutineEventRecord) *RoutineEventRecord {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Event = cloneRoutineEvent(record.Event)
	return &clone
}

func cloneRoutineEvent(event RoutineEvent) RoutineEvent {
	clone := event
	if event.Fields != nil {
		clone.Fields = make(map[string][]string, len(event.Fields))
		for key, values := range event.Fields {
			clone.Fields[key] = append([]string(nil), values...)
		}
	}
	if event.Payload != nil {
		clone.Payload = append(json.RawMessage(nil), event.Payload...)
	}
	return clone
}

func cloneRoutineEventBatch(batch *RoutineEventBatch) *RoutineEventBatch {
	if batch == nil {
		return nil
	}
	clone := *batch
	clone.EventIDs = append([]string(nil), batch.EventIDs...)
	if batch.StartedAt != nil {
		started := *batch.StartedAt
		clone.StartedAt = &started
	}
	return &clone
}

func cloneHook(hook *Hook) *Hook {
	if hook == nil {
		return nil
	}
	clone := *hook
	return &clone
}

// indexRoutineEventActivationLocked keeps the in-memory event index in step
// with an activation write. The caller holds s.mu.
func (s *MemoryStore) indexRoutineEventActivationLocked(previous, next *RunbookActivation) {
	if previous != nil {
		if kind, sourceRef := routineEventIndexOf(previous); kind != "" {
			key := RoutineEventSourceKey{Scope: previous.Scope, Kind: kind, SourceRef: sourceRef}
			delete(s.routineEventActivations[key], previous.ID)
			if len(s.routineEventActivations[key]) == 0 {
				delete(s.routineEventActivations, key)
			}
		}
	}
	if next == nil || next.Status != RunbookActivationActive {
		return
	}
	kind, sourceRef := routineEventIndexOf(next)
	if kind == "" {
		return
	}
	key := RoutineEventSourceKey{Scope: next.Scope, Kind: kind, SourceRef: sourceRef}
	if s.routineEventActivations[key] == nil {
		s.routineEventActivations[key] = make(map[string]struct{})
	}
	s.routineEventActivations[key][next.ID] = struct{}{}
}

func (s *MemoryStore) ListRoutineEventActivations(_ context.Context, scope Scope, kind RoutineEventKind, sourceRef string) ([]*RunbookActivation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.routineEventActivations[RoutineEventSourceKey{Scope: scope, Kind: kind, SourceRef: sourceRef}]
	values := make([]*RunbookActivation, 0, len(ids))
	for id := range ids {
		if activation := s.runbookActivations[portfolioKey(scope, id)]; activation != nil {
			values = append(values, cloneRunbookActivation(activation))
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return values, nil
}

func (s *MemoryStore) ListRoutineEventSubscriptions(_ context.Context, kinds []RoutineEventKind, after string, limit int) ([]RoutineEventSubscription, string, error) {
	if _, err := routineEventKindStrings(kinds); err != nil {
		return nil, "", err
	}
	afterKind, afterScope, afterID, err := decodeRoutineEventSubscriptionCursor(after)
	if err != nil {
		return nil, "", err
	}
	limit = normalizeRoutineEventLimit(limit, routineEventSubscriptionPageLimit)
	wanted := make(map[RoutineEventKind]bool, len(kinds))
	for _, kind := range kinds {
		wanted[kind] = true
	}
	s.mu.RLock()
	values := make([]RoutineEventSubscription, 0)
	for key, ids := range s.routineEventActivations {
		if len(kinds) > 0 && !wanted[key.Kind] {
			continue
		}
		for id := range ids {
			if subscription, ok := routineEventSubscriptionOf(s.runbookActivations[portfolioKey(key.Scope, id)]); ok {
				values = append(values, subscription)
			}
		}
	}
	s.mu.RUnlock()
	less := func(a RoutineEventSubscription, kind, scopeID, id string) bool {
		if a.Scope.Kind != kind {
			return a.Scope.Kind < kind
		}
		if a.Scope.ID != scopeID {
			return a.Scope.ID < scopeID
		}
		return a.ActivationID < id
	}
	sort.Slice(values, func(i, j int) bool {
		return less(values[i], values[j].Scope.Kind, values[j].Scope.ID, values[j].ActivationID)
	})
	page := make([]RoutineEventSubscription, 0, limit)
	for _, value := range values {
		if after != "" && !less(RoutineEventSubscription{Scope: Scope{Kind: afterKind, ID: afterScope}, ActivationID: afterID}, value.Scope.Kind, value.Scope.ID, value.ActivationID) {
			continue
		}
		page = append(page, value)
		if len(page) == limit {
			return page, encodeRoutineEventSubscriptionCursor(value), nil
		}
	}
	return page, "", nil
}

func (s *MemoryStore) ListRoutineEventSourceKeysInScope(_ context.Context, scope Scope) ([]RoutineEventSourceKey, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]RoutineEventSourceKey, 0)
	for key, ids := range s.routineEventActivations {
		if key.Scope == scope && len(ids) > 0 {
			keys = append(keys, key)
		}
	}
	sortRoutineEventSourceKeys(keys)
	return keys, nil
}

func (s *MemoryStore) CountActiveRoutineEventRuns(_ context.Context, scope Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	since := time.Now().Add(-RoutineEventActiveRunWindow)
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for key, batch := range s.routineEventBatches {
		if key.scope != scope || batch.StartedAt == nil || batch.StartedAt.Before(since) {
			continue
		}
		if run := s.agentRuns[portfolioKey(scope, batch.RunID)]; run != nil && !isTerminalAgentRunStatus(run.Status) {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) InsertRoutineEvent(_ context.Context, record *RoutineEventRecord) (bool, error) {
	if err := prepareRoutineEventRecord(record); err != nil {
		return false, err
	}
	stored := cloneRoutineEventRecord(record)
	if stored.State == RoutineEventDropped {
		stored.Event = RoutineEvent{ID: record.Event.ID, Test: record.Event.Test, OccurredAt: record.Event.OccurredAt.UTC()}
	}
	key := memoryRoutineEventKey{scope: record.Scope, activationID: record.ActivationID, eventID: record.Event.ID}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.routineEvents[key]; exists {
		return false, nil
	}
	s.routineEvents[key] = stored
	return true, nil
}

func routineEventWaiting(record *RoutineEventRecord) bool {
	return record.State == RoutineEventPending || record.State == RoutineEventBatched
}

func (s *MemoryStore) CountPendingRoutineEvents(_ context.Context, scope Scope, activationID string) (int, error) {
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for key, record := range s.routineEvents {
		if key.scope == scope && key.activationID == activationID && routineEventWaiting(record) {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) CountPendingRoutineEventsInScope(_ context.Context, scope Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for key, record := range s.routineEvents {
		if key.scope == scope && routineEventWaiting(record) {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) ListRoutineEventWork(_ context.Context, scope Scope, limit int) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	limit = normalizeRoutineEventLimit(limit, routineEventSubscriptionPageLimit)
	s.mu.RLock()
	unique := make(map[string]struct{})
	for key, record := range s.routineEvents {
		if key.scope == scope && routineEventWaiting(record) {
			unique[key.activationID] = struct{}{}
		}
	}
	s.mu.RUnlock()
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// openRoutineEventBatchLocked returns the unstarted batch of an activation.
func (s *MemoryStore) openRoutineEventBatchLocked(scope Scope, activationID string) *RoutineEventBatch {
	for key, batch := range s.routineEventBatches {
		if key.scope == scope && batch.ActivationID == activationID && batch.StartedAt == nil {
			return batch
		}
	}
	return nil
}

func (s *MemoryStore) ClaimRoutineEventBatch(_ context.Context, scope Scope, activationID string, maxEvents int, now time.Time) (*RoutineEventBatch, error) {
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return nil, err
	}
	maxEvents = normalizeRoutineEventBatchSize(maxEvents)
	s.mu.Lock()
	defer s.mu.Unlock()
	if open := s.openRoutineEventBatchLocked(scope, activationID); open != nil {
		return cloneRoutineEventBatch(open), nil
	}
	pending := func(test bool) []*RoutineEventRecord {
		values := make([]*RoutineEventRecord, 0)
		for key, record := range s.routineEvents {
			if key.scope == scope && key.activationID == activationID && record.State == RoutineEventPending && record.Event.Test == test {
				values = append(values, record)
			}
		}
		sort.Slice(values, func(i, j int) bool {
			if !values[i].ReceivedAt.Equal(values[j].ReceivedAt) {
				return values[i].ReceivedAt.Before(values[j].ReceivedAt)
			}
			return values[i].Event.ID < values[j].Event.ID
		})
		if len(values) > maxEvents {
			values = values[:maxEvents]
		}
		return values
	}
	test := true
	records := pending(true)
	if len(records) == 0 {
		test, records = false, pending(false)
	}
	if len(records) == 0 {
		return nil, nil
	}
	batch := &RoutineEventBatch{Scope: scope, ID: newRoutineEventBatchID(), ActivationID: activationID, Test: test, CreatedAt: now.UTC()}
	if !test {
		var previous time.Time
		for key, existing := range s.routineEventBatches {
			if key.scope == scope && existing.ActivationID == activationID && !existing.Test && existing.CreatedAt.After(previous) {
				previous = existing.CreatedAt
			}
		}
		for key, record := range s.routineEvents {
			if key.scope == scope && key.activationID == activationID && record.State == RoutineEventDropped &&
				record.DroppedReason == RoutineEventDroppedOverflow && !record.Event.Test && record.ReceivedAt.After(previous) {
				batch.Dropped++
			}
		}
	}
	for _, record := range records {
		record.State, record.BatchID = RoutineEventBatched, batch.ID
		batch.EventIDs = append(batch.EventIDs, record.Event.ID)
	}
	s.routineEventBatches[memoryRoutineEventBatchKey{scope: scope, id: batch.ID}] = batch
	return cloneRoutineEventBatch(batch), nil
}

func (s *MemoryStore) MarkRoutineEventBatchStarted(_ context.Context, scope Scope, batchID, runID string, now time.Time) error {
	if err := validateRoutineEventScopeActivation(scope, batchID); err != nil {
		return err
	}
	if runID == "" {
		return errors.New("a started routine event batch needs its Run")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.routineEventBatches[memoryRoutineEventBatchKey{scope: scope, id: batchID}]
	if batch == nil {
		return ErrRoutineEventBatchNotFound
	}
	if batch.StartedAt != nil {
		if batch.RunID == runID {
			return nil
		}
		return ErrRoutineEventBatchStarted
	}
	started := now.UTC()
	batch.StartedAt, batch.RunID = &started, runID
	for _, eventID := range batch.EventIDs {
		if record := s.routineEvents[memoryRoutineEventKey{scope: scope, activationID: batch.ActivationID, eventID: eventID}]; record != nil && record.State == RoutineEventBatched {
			record.State = RoutineEventStarted
		}
	}
	return nil
}

func (s *MemoryStore) GetRoutineEventBatch(_ context.Context, scope Scope, batchID string) (*RoutineEventBatch, []*RoutineEventRecord, error) {
	if err := validateRoutineEventScopeActivation(scope, batchID); err != nil {
		return nil, nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	batch := s.routineEventBatches[memoryRoutineEventBatchKey{scope: scope, id: batchID}]
	if batch == nil {
		return nil, nil, nil
	}
	records := make([]*RoutineEventRecord, 0, len(batch.EventIDs))
	for _, eventID := range batch.EventIDs {
		if record := s.routineEvents[memoryRoutineEventKey{scope: scope, activationID: batch.ActivationID, eventID: eventID}]; record != nil {
			records = append(records, cloneRoutineEventRecord(record))
		}
	}
	return cloneRoutineEventBatch(batch), records, nil
}

func (s *MemoryStore) GetOpenRoutineEventBatch(_ context.Context, scope Scope, activationID string) (*RoutineEventBatch, error) {
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRoutineEventBatch(s.openRoutineEventBatchLocked(scope, activationID)), nil
}

func (s *MemoryStore) CountRoutineEventBatchesSince(_ context.Context, scope Scope, activationID string, since time.Time, test bool) (int, *time.Time, error) {
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	var oldest *time.Time
	for key, batch := range s.routineEventBatches {
		if key.scope != scope || batch.ActivationID != activationID || batch.Test != test || batch.StartedAt == nil || batch.StartedAt.Before(since) {
			continue
		}
		count++
		if oldest == nil || batch.StartedAt.Before(*oldest) {
			started := *batch.StartedAt
			oldest = &started
		}
	}
	return count, oldest, nil
}

func (s *MemoryStore) CountRoutineTestEventsSince(_ context.Context, scope Scope, activationID string, since time.Time) (int, error) {
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for key, record := range s.routineEvents {
		if key.scope == scope && key.activationID == activationID && record.Event.Test && !record.ReceivedAt.Before(since) {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) DropRoutineEvents(_ context.Context, scope Scope, activationID string, reason string) (int, error) {
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	if !validRoutineEventDropReason(reason) {
		return 0, fmt.Errorf("routine event dropped reason %q is invalid", reason)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for key, record := range s.routineEvents {
		if key.scope != scope || key.activationID != activationID || !routineEventWaiting(record) {
			continue
		}
		record.State, record.DroppedReason, record.BatchID = RoutineEventDropped, reason, ""
		record.Event = RoutineEvent{ID: record.Event.ID, Test: record.Event.Test, OccurredAt: record.Event.OccurredAt}
		count++
	}
	if open := s.openRoutineEventBatchLocked(scope, activationID); open != nil {
		delete(s.routineEventBatches, memoryRoutineEventBatchKey{scope: scope, id: open.ID})
	}
	return count, nil
}

func (s *MemoryStore) RoutineEventActivity(_ context.Context, scope Scope, activationIDs []string, now time.Time) (map[string]RoutineEventActivity, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	ids := uniqueRoutineActivationIDs(activationIDs)
	dayAgo, hourAgo := now.Add(-24*time.Hour), now.Add(-time.Hour)
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]RoutineEventActivity, len(ids))
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
		result[id] = RoutineEventActivity{ActivationID: id}
	}
	oldestInHour := make(map[string]*time.Time)
	for key, batch := range s.routineEventBatches {
		if key.scope != scope || !wanted[batch.ActivationID] || batch.Test || batch.StartedAt == nil {
			continue
		}
		activity := result[batch.ActivationID]
		started := batch.StartedAt.UTC()
		if activity.LastFiredAt == nil || started.After(*activity.LastFiredAt) {
			activity.LastFiredAt = &started
		}
		if !started.Before(dayAgo) {
			activity.FiredLast24h++
		}
		if !started.Before(hourAgo) {
			activity.FiredLastHour++
			if oldest := oldestInHour[batch.ActivationID]; oldest == nil || started.Before(*oldest) {
				oldestInHour[batch.ActivationID] = &started
			}
		}
		result[batch.ActivationID] = activity
	}
	for key, record := range s.routineEvents {
		if key.scope != scope || !wanted[key.activationID] {
			continue
		}
		activity := result[key.activationID]
		if routineEventWaiting(record) {
			activity.Pending++
		} else if record.State == RoutineEventDropped && !record.ReceivedAt.Before(dayAgo) {
			activity.DroppedLast24h++
		}
		result[key.activationID] = activity
	}
	for _, id := range ids {
		activity := result[id]
		finishRoutineEventActivity(&activity, routineEventMaxPerHour(s.runbookActivations[portfolioKey(scope, id)]), oldestInHour[id])
		result[id] = activity
	}
	return result, nil
}

// routineActivationGoneLocked reports whether an activation is missing, or
// retired with its last change before cutoff.
func (s *MemoryStore) routineActivationGoneLocked(scope Scope, activationID string, cutoff time.Time) bool {
	activation := s.runbookActivations[portfolioKey(scope, activationID)]
	return activation == nil || activation.Status == RunbookActivationRetired && activation.UpdatedAt.Before(cutoff)
}

func (s *MemoryStore) PruneRoutineEvents(_ context.Context, before time.Time, limit int) (int, error) {
	limit = normalizeRoutineEventLimit(limit, 10_000)
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for key, record := range s.routineEvents {
		if removed == limit {
			return removed, nil
		}
		if (record.State == RoutineEventStarted || record.State == RoutineEventDropped) && record.ReceivedAt.Before(before) {
			delete(s.routineEvents, key)
			removed++
		}
	}
	for key, batch := range s.routineEventBatches {
		if removed == limit {
			return removed, nil
		}
		if batch.CreatedAt.Before(before) && s.routineActivationGoneLocked(key.scope, batch.ActivationID, before) {
			delete(s.routineEventBatches, key)
			removed++
		}
	}
	return removed, nil
}

func (s *MemoryStore) CreateHook(_ context.Context, hook *Hook) (*Hook, error) {
	value, err := prepareHook(hook)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.hooks[value.ID]; existing != nil {
		if !sameHookOwner(existing, value) {
			return nil, ErrHookConflict
		}
		return cloneHook(existing), nil
	}
	for _, existing := range s.hooks {
		if sameHookOwner(existing, value) {
			return nil, ErrHookConflict
		}
	}
	s.hooks[value.ID] = value
	return cloneHook(value), nil
}

func (s *MemoryStore) GetHook(_ context.Context, hookID string) (*Hook, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneHook(s.hooks[hookID]), nil
}

func (s *MemoryStore) UpdateHook(_ context.Context, scope Scope, hookID string, expectedRevision int64, rotate bool, pathTokenEnabled *bool, now time.Time) (*Hook, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hook := s.hooks[hookID]
	if hook == nil || hook.Scope != scope {
		return nil, ErrHookNotFound
	}
	if hook.Revision != expectedRevision {
		return nil, ErrHookRevision
	}
	if rotate {
		hook.Revision++
		hook.RotatedAt = now.UTC()
	}
	if pathTokenEnabled != nil {
		hook.PathTokenEnabled = *pathTokenEnabled
	}
	return cloneHook(hook), nil
}

func (s *MemoryStore) PruneHooks(_ context.Context, scope Scope, createdBefore time.Time) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, hook := range s.hooks {
		if hook.Scope != scope || !hook.CreatedAt.Before(createdBefore) {
			continue
		}
		activation := s.runbookActivations[portfolioKey(scope, hook.ActivationID)]
		if activation == nil || activation.Status == RunbookActivationRetired {
			delete(s.hooks, id)
			removed++
		}
	}
	return removed, nil
}
