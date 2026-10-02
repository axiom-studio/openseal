package runtime

import "time"

type memoryRunEventOrderKey struct {
	ReceivedAt time.Time
	Identity   memoryRunEventReceiptKey
}

// AVL indexes provide bounded chronological retention reads without scanning
// or sorting the inbox. Separate scope roots make scoped maintenance bounded as
// well; both indexes change under the same mutex as receipt publication.
type memoryRunEventOrderNode = memoryOrderedIndexNode[memoryRunEventOrderKey]

func memoryRunEventOrderLess(left, right memoryRunEventOrderKey) bool {
	if !left.ReceivedAt.Equal(right.ReceivedAt) {
		return left.ReceivedAt.Before(right.ReceivedAt)
	}
	if left.Identity.Scope.Kind != right.Identity.Scope.Kind {
		return left.Identity.Scope.Kind < right.Identity.Scope.Kind
	}
	if left.Identity.Scope.ID != right.Identity.Scope.ID {
		return left.Identity.Scope.ID < right.Identity.Scope.ID
	}
	if left.Identity.Source != right.Identity.Source {
		return left.Identity.Source < right.Identity.Source
	}
	return left.Identity.ID < right.Identity.ID
}

func memoryRunEventOrderInsert(node *memoryRunEventOrderNode, key memoryRunEventOrderKey) *memoryRunEventOrderNode {
	return memoryOrderedIndexInsert(node, key, memoryRunEventOrderLess)
}

func memoryRunEventOrderDelete(node *memoryRunEventOrderNode, key memoryRunEventOrderKey) *memoryRunEventOrderNode {
	return memoryOrderedIndexDelete(node, key, memoryRunEventOrderLess)
}

func (s *MemoryStore) insertMemoryRunEventReceiptOrderLocked(receipt *RunEventReceipt) {
	key := memoryRunEventOrderKey{ReceivedAt: receipt.ReceivedAt, Identity: memoryRunEventReceiptIdentity(receipt)}
	s.runEventReceiptOrder = memoryRunEventOrderInsert(s.runEventReceiptOrder, key)
	s.runEventReceiptScopeOrder[key.Identity.Scope] = memoryRunEventOrderInsert(s.runEventReceiptScopeOrder[key.Identity.Scope], key)
}

func (s *MemoryStore) memoryRunEventReceiptsAfterLocked(after *memoryRunEventOrderKey, before time.Time, limit int, scope *Scope) []memoryRunEventOrderKey {
	root := s.runEventReceiptOrder
	if scope != nil {
		root = s.runEventReceiptScopeOrder[*scope]
	}
	return memoryOrderedIndexAfter(root, after, func(key memoryRunEventOrderKey) bool {
		return key.ReceivedAt.Before(before)
	}, limit, memoryRunEventOrderLess)
}

func (s *MemoryStore) memoryRunEventReceiptProtectedLocked(receipt *RunEventReceipt) bool {
	identity := memoryRunEventReceiptIdentity(receipt)
	for key := range s.runEventWaitIndex[memoryRunEventReceiptSelector(receipt)] {
		wait := s.runEventWaits[key]
		if _, consumed := s.runEventConsumptions[identity][key.memoryRunEventRunKey]; consumed {
			continue
		}
		if (wait.Status == RunEventWaitPending || wait.Status == RunEventWaitPaused) && runEventWaitMatches(wait, receipt) {
			return true
		}
	}
	return false
}

func (s *MemoryStore) deleteMemoryRunEventReceiptLocked(identity memoryRunEventReceiptKey) {
	receipt := s.runEventReceipts[identity]
	if receipt == nil {
		return
	}
	selector := memoryRunEventReceiptSelector(receipt)
	delete(s.runEventReceiptIndex[selector], identity)
	if len(s.runEventReceiptIndex[selector]) == 0 {
		delete(s.runEventReceiptIndex, selector)
	}
	key := memoryRunEventOrderKey{ReceivedAt: receipt.ReceivedAt, Identity: identity}
	s.runEventReceiptOrder = memoryRunEventOrderDelete(s.runEventReceiptOrder, key)
	root := memoryRunEventOrderDelete(s.runEventReceiptScopeOrder[identity.Scope], key)
	if root == nil {
		delete(s.runEventReceiptScopeOrder, identity.Scope)
		delete(s.runEventReceiptPruneCursors, identity.Scope)
	} else {
		s.runEventReceiptScopeOrder[identity.Scope] = root
	}
	delete(s.runEventReceipts, identity)
	s.deleteMemoryRunEventNotificationLocked(identity)
	// Consumption tombstones intentionally outlive event payloads: republishing
	// an archived provider event must not satisfy a later step in the same Run.
}
