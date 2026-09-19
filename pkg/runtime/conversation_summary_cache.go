package runtime

import (
	"container/list"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"
)

func validSummaryIdentity(scope Scope, conversationID, viewerKey string) bool {
	b, err := hex.DecodeString(viewerKey)
	return scope.Validate() == nil && validOpaqueIdentifier(conversationID, 128) && err == nil && len(b) == 32
}

func (summary *conversationSummary) Validate() error {
	if summary == nil || !validSummaryIdentity(summary.Scope, summary.ConversationID, summary.ViewerKey) || summary.ThroughSequence < 1 || strings.TrimSpace(summary.Text) == "" || len(summary.Text) > conversationSummaryMaximumBytes || !utf8.ValidString(summary.Text) {
		return errors.New("invalid conversation summary")
	}
	b, err := hex.DecodeString(summary.Basis)
	if err != nil || len(b) != 32 {
		return errors.New("invalid conversation summary basis")
	}
	return nil
}

const conversationSummaryCacheCapacity = 256

type conversationSummaryCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
}
type conversationSummaryCacheEntry struct {
	key     string
	summary conversationSummary
}

func summaryCacheKey(scope Scope, conversationID, viewerKey string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + conversationID + "\x00" + viewerKey
}
func (cache *conversationSummaryCache) get(scope Scope, conversationID, viewerKey string) *conversationSummary {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[summaryCacheKey(scope, conversationID, viewerKey)]
	if entry == nil {
		return nil
	}
	cache.order.MoveToFront(entry)
	copy := entry.Value.(conversationSummaryCacheEntry).summary
	return &copy
}
func (cache *conversationSummaryCache) put(summary *conversationSummary) {
	if summary.Validate() != nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = make(map[string]*list.Element)
	}
	key := summaryCacheKey(summary.Scope, summary.ConversationID, summary.ViewerKey)
	if entry := cache.entries[key]; entry != nil {
		if entry.Value.(conversationSummaryCacheEntry).summary.ThroughSequence < summary.ThroughSequence {
			entry.Value = conversationSummaryCacheEntry{key, *summary}
		}
		cache.order.MoveToFront(entry)
		return
	}
	cache.entries[key] = cache.order.PushFront(conversationSummaryCacheEntry{key, *summary})
	if cache.order.Len() > conversationSummaryCacheCapacity {
		oldest := cache.order.Back()
		delete(cache.entries, oldest.Value.(conversationSummaryCacheEntry).key)
		cache.order.Remove(oldest)
	}
}
