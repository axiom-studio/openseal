package runtime

import (
	"context"
	"fmt"
)

// MaximumChannelMessageBatchSize bounds optional exact-ID store reads. A
// channel poll chunks larger selections rather than enlarging the SQL query.
const MaximumChannelMessageBatchSize = 200

// ChannelMessageBatchStore is an optional extension to ConversationStore.
// Reads return only the exact scope, conversation and message IDs requested;
// missing messages are omitted, and result order is unspecified. Callers still
// apply the current viewer's audience and canonical ancestry visibility checks.
type ChannelMessageBatchStore interface {
	GetChannelMessagesByIDs(context.Context, Scope, string, []string) ([]*ChannelMessage, error)
}

var (
	_ ChannelMessageBatchStore = (*MemoryStore)(nil)
	_ ChannelMessageBatchStore = (*SQLiteStore)(nil)
	_ ChannelMessageBatchStore = (*PostgresStore)(nil)
)

func validateChannelMessageBatch(scope Scope, ids []string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if len(ids) > MaximumChannelMessageBatchSize {
		return fmt.Errorf("%w: message batch must contain at most %d IDs", ErrInvalidConversation, MaximumChannelMessageBatchSize)
	}
	return nil
}

func loadChannelMessageBatches(ctx context.Context, store ChannelMessageBatchStore, scope Scope, conversationID string, ids []string) (map[string]*ChannelMessage, error) {
	unique := make([]string, 0, len(ids))
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !requested[id] {
			requested[id] = true
			unique = append(unique, id)
		}
	}
	result := make(map[string]*ChannelMessage, len(unique))
	for offset := 0; offset < len(unique); offset += MaximumChannelMessageBatchSize {
		end := min(offset+MaximumChannelMessageBatchSize, len(unique))
		messages, err := store.GetChannelMessagesByIDs(ctx, scope, conversationID, unique[offset:end])
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			if message != nil && message.Scope == scope && message.ConversationID == conversationID && requested[message.ID] {
				result[message.ID] = message
			}
		}
	}
	return result, nil
}

// Visibility is recomputed for every request. Parent reads are staged so a
// hidden or missing thread root never causes its reply parent to be read.
// Ordinary parents retain one-level checks. StartsThread anchors additionally
// preserve their bounded ancestor restrictions without walking transcripts.
func filterChannelMessagesWithBatchParents(ctx context.Context, store ChannelMessageBatchStore, scope Scope, conversationID string, messages []*ChannelMessage, viewer ConversationViewer) ([]*ChannelMessage, error) {
	threadIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		if CanViewChannelMessage(message, viewer) && message.ThreadRootID != "" && message.ThreadRootID != message.ID {
			threadIDs = append(threadIDs, message.ThreadRootID)
		}
	}
	threads, err := loadChannelMessageBatches(ctx, store, scope, conversationID, threadIDs)
	if err != nil {
		return nil, err
	}
	threadVisible := make([]*ChannelMessage, 0, len(messages))
	replyIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		if !CanViewChannelMessage(message, viewer) {
			continue
		}
		if message.ThreadRootID != "" && message.ThreadRootID != message.ID && !CanViewChannelMessage(threads[message.ThreadRootID], viewer) {
			continue
		}
		threadVisible = append(threadVisible, message)
		if linked := message.ReplyToMessageID; linked != "" && linked != message.ID && linked != message.ThreadRootID {
			replyIDs = append(replyIDs, linked)
		}
	}
	replies, err := loadChannelMessageBatches(ctx, store, scope, conversationID, replyIDs)
	if err != nil {
		return nil, err
	}
	visible := make([]*ChannelMessage, 0, len(threadVisible))
	for _, message := range threadVisible {
		if linked := message.ReplyToMessageID; linked != "" && linked != message.ID && linked != message.ThreadRootID && !CanViewChannelMessage(replies[linked], viewer) {
			continue
		}
		visible = append(visible, message)
	}
	hasThreadStart := false
	for _, message := range visible {
		if message.StartsThread {
			hasThreadStart = true
			break
		}
		for _, id := range [2]string{message.ThreadRootID, message.ReplyToMessageID} {
			parent := threads[id]
			if parent == nil {
				parent = replies[id]
			}
			if parent != nil && parent.StartsThread {
				hasThreadStart = true
				break
			}
		}
	}
	if !hasThreadStart {
		return visible, nil
	}
	known := make(map[string]*ChannelMessage, len(messages)+len(threads)+len(replies))
	for _, message := range messages {
		if message != nil {
			known[message.ID] = message
		}
	}
	for id, message := range threads {
		known[id] = message
	}
	for id, message := range replies {
		known[id] = message
	}
	return filterChannelMessageThreadStarts(ctx, scope, conversationID, visible, known,
		func(ctx context.Context, ids []string) (map[string]*ChannelMessage, error) {
			return loadChannelMessageBatches(ctx, store, scope, conversationID, ids)
		}, viewer)
}
