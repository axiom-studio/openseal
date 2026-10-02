package runtime

import "time"

type memoryRunEventWaitDueKey struct {
	AvailableAt time.Time
	Wait        memoryRunEventWaitKey
}

type memoryRunEventNotificationDueKey struct {
	AvailableAt time.Time
	Receipt     memoryRunEventReceiptKey
}

type memoryRunEventScopeDueKey struct {
	AvailableAt time.Time
	Scope       Scope
}

func memoryRunEventWaitKeyLess(left, right memoryRunEventWaitKey) bool {
	if left.RunID != right.RunID {
		return left.RunID < right.RunID
	}
	return left.Key < right.Key
}

func memoryRunEventWaitDueLess(left, right memoryRunEventWaitDueKey) bool {
	if !left.AvailableAt.Equal(right.AvailableAt) {
		return left.AvailableAt.Before(right.AvailableAt)
	}
	return memoryRunEventWaitKeyLess(left.Wait, right.Wait)
}

func memoryRunEventNotificationDueLess(left, right memoryRunEventNotificationDueKey) bool {
	if !left.AvailableAt.Equal(right.AvailableAt) {
		return left.AvailableAt.Before(right.AvailableAt)
	}
	if left.Receipt.Source != right.Receipt.Source {
		return left.Receipt.Source < right.Receipt.Source
	}
	return left.Receipt.ID < right.Receipt.ID
}

func memoryRunEventScopeDueLess(left, right memoryRunEventScopeDueKey) bool {
	if !left.AvailableAt.Equal(right.AvailableAt) {
		return left.AvailableAt.Before(right.AvailableAt)
	}
	if left.Scope.Kind != right.Scope.Kind {
		return left.Scope.Kind < right.Scope.Kind
	}
	return left.Scope.ID < right.Scope.ID
}

func (s *MemoryStore) removeMemoryRunEventWaitOrderedIndexesLocked(key memoryRunEventWaitKey, wait *RunEventWait) {
	selector := memoryRunEventWaitSelector(wait)
	root := memoryOrderedIndexDelete(s.runEventWaitSelectorOrder[selector], key, memoryRunEventWaitKeyLess)
	if root == nil {
		delete(s.runEventWaitSelectorOrder, selector)
	} else {
		s.runEventWaitSelectorOrder[selector] = root
	}
	if dueKey, exists := s.runEventWaitDueKeys[key]; exists {
		root := memoryOrderedIndexDelete(s.runEventWaitDueOrder[key.Scope], dueKey, memoryRunEventWaitDueLess)
		if root == nil {
			delete(s.runEventWaitDueOrder, key.Scope)
		} else {
			s.runEventWaitDueOrder[key.Scope] = root
		}
		delete(s.runEventWaitDueKeys, key)
	}
}

func (s *MemoryStore) insertMemoryRunEventWaitOrderedIndexesLocked(key memoryRunEventWaitKey, wait *RunEventWait) {
	if wait.Status != RunEventWaitPending {
		return
	}
	selector := memoryRunEventWaitSelector(wait)
	s.runEventWaitSelectorOrder[selector] = memoryOrderedIndexInsert(s.runEventWaitSelectorOrder[selector], key, memoryRunEventWaitKeyLess)
	if wait.AvailableAt == nil {
		return
	}
	available := *wait.AvailableAt
	if wait.LeaseExpiresAt != nil && wait.LeaseExpiresAt.After(available) {
		available = *wait.LeaseExpiresAt
	}
	dueKey := memoryRunEventWaitDueKey{AvailableAt: available, Wait: key}
	s.runEventWaitDueKeys[key] = dueKey
	s.runEventWaitDueOrder[key.Scope] = memoryOrderedIndexInsert(s.runEventWaitDueOrder[key.Scope], dueKey, memoryRunEventWaitDueLess)
}

func (s *MemoryStore) setMemoryRunEventScopeWorkLocked(scope Scope, available *time.Time) {
	if previous, exists := s.runEventWaitScopeWork[scope]; exists {
		key := memoryRunEventScopeDueKey{AvailableAt: previous, Scope: scope}
		s.runEventWaitScopeOrder = memoryOrderedIndexDelete(s.runEventWaitScopeOrder, key, memoryRunEventScopeDueLess)
	}
	if available == nil {
		delete(s.runEventWaitScopeWork, scope)
		return
	}
	s.runEventWaitScopeWork[scope] = *available
	key := memoryRunEventScopeDueKey{AvailableAt: *available, Scope: scope}
	s.runEventWaitScopeOrder = memoryOrderedIndexInsert(s.runEventWaitScopeOrder, key, memoryRunEventScopeDueLess)
}

func (s *MemoryStore) refreshMemoryRunEventScopeWorkLocked(scope Scope) {
	var available *time.Time
	if due, exists := memoryOrderedIndexFirst(s.runEventWaitDueOrder[scope]); exists {
		at := due.AvailableAt
		available = &at
	}
	if notification, exists := memoryOrderedIndexFirst(s.runEventNotificationOrder[scope]); exists &&
		(available == nil || notification.AvailableAt.Before(*available)) {
		at := notification.AvailableAt
		available = &at
	}
	s.setMemoryRunEventScopeWorkLocked(scope, available)
}

func (s *MemoryStore) saveMemoryRunEventNotificationLocked(notification runEventNotification) {
	identity := memoryRunEventReceiptKey{Scope: notification.Scope, Source: notification.Source, ID: notification.EventID}
	if old, exists := s.runEventWaitNotifications[identity]; exists {
		key := memoryRunEventNotificationDueKey{AvailableAt: old.AvailableAt, Receipt: identity}
		s.runEventNotificationOrder[identity.Scope] = memoryOrderedIndexDelete(s.runEventNotificationOrder[identity.Scope], key, memoryRunEventNotificationDueLess)
	}
	s.runEventWaitNotifications[identity] = notification
	key := memoryRunEventNotificationDueKey{AvailableAt: notification.AvailableAt, Receipt: identity}
	s.runEventNotificationOrder[identity.Scope] = memoryOrderedIndexInsert(s.runEventNotificationOrder[identity.Scope], key, memoryRunEventNotificationDueLess)
	s.refreshMemoryRunEventScopeWorkLocked(identity.Scope)
}

func (s *MemoryStore) deleteMemoryRunEventNotificationLocked(identity memoryRunEventReceiptKey) {
	if old, exists := s.runEventWaitNotifications[identity]; exists {
		key := memoryRunEventNotificationDueKey{AvailableAt: old.AvailableAt, Receipt: identity}
		root := memoryOrderedIndexDelete(s.runEventNotificationOrder[identity.Scope], key, memoryRunEventNotificationDueLess)
		if root == nil {
			delete(s.runEventNotificationOrder, identity.Scope)
		} else {
			s.runEventNotificationOrder[identity.Scope] = root
		}
		delete(s.runEventWaitNotifications, identity)
		s.refreshMemoryRunEventScopeWorkLocked(identity.Scope)
	}
}

// A claim advances a bounded descriptor batch and a shared wait-candidate
// budget. Empty selectors are cheap to drain, while a large fanout cannot
// monopolize the store mutex or block other ready observations indefinitely.
func (s *MemoryStore) notifyMemoryRunEventWaitsLocked(scope Scope, now time.Time) {
	remaining := runEventNotificationPageLimit
	for descriptor := 0; descriptor < runEventNotificationBatchLimit && remaining > 0; descriptor++ {
		key, exists := memoryOrderedIndexFirst(s.runEventNotificationOrder[scope])
		if !exists || key.AvailableAt.After(now) {
			return
		}
		notification := s.runEventWaitNotifications[key.Receipt]
		receipt := s.runEventReceipts[key.Receipt]
		if receipt == nil {
			s.deleteMemoryRunEventNotificationLocked(key.Receipt)
			continue
		}
		var after *memoryRunEventWaitKey
		if notification.AfterRunID != "" {
			cursor := memoryRunEventWaitKey{memoryRunEventRunKey{Scope: scope, RunID: notification.AfterRunID}, notification.AfterWaitKey}
			after = &cursor
		}
		selector := memoryRunEventReceiptSelector(receipt)
		page := memoryOrderedIndexAfter(s.runEventWaitSelectorOrder[selector], after, nil, remaining, memoryRunEventWaitKeyLess)
		for _, waitKey := range page {
			wait := s.runEventWaits[waitKey]
			if _, consumed := s.runEventConsumptions[key.Receipt][waitKey.memoryRunEventRunKey]; consumed || !runEventWaitMatches(wait, receipt) {
				continue
			}
			if wait.AvailableAt == nil || now.Before(*wait.AvailableAt) {
				updated := cloneRunEventWait(wait)
				at := now
				updated.AvailableAt = &at
				s.saveMemoryRunEventWaitLocked(waitKey, updated)
			}
		}
		if len(page) == remaining {
			last := page[len(page)-1]
			notification.AfterRunID, notification.AfterWaitKey = last.RunID, last.Key
			notification.AvailableAt = now
			s.saveMemoryRunEventNotificationLocked(notification)
		} else {
			s.deleteMemoryRunEventNotificationLocked(key.Receipt)
		}
		remaining -= len(page)
	}
}
