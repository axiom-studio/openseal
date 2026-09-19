package runtime

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"
)

// ConversationHistoryReadStore must enforce Viewer on both reads. This is a
// provider-neutral kernel contract; hosts derive scope/owner/viewer from a run.
type ConversationHistoryReadStore interface {
	GetConversation(context.Context, Scope, string) (*Conversation, error)
	ListChannelMessages(context.Context, ChannelMessageFilter) ([]*ChannelMessage, error)
	GetVisibleChannelMessage(context.Context, Scope, string, string, ConversationViewer) (*ChannelMessage, error)
}

type ConversationHistoryReadRequest struct {
	MessageID      string `json:"messageId,omitempty"`
	BeforeSequence int64  `json:"beforeSequence,omitempty"`
	OffsetBytes    int    `json:"offsetBytes,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

type ConversationHistoryExcerpt struct {
	CreatedAt        time.Time                 `json:"createdAt"`
	Intent           ConversationMessageIntent `json:"intent"`
	ReplyToMessageID string                    `json:"replyToMessageId,omitempty"`
	ID               string                    `json:"id"`
	Sequence         int64                     `json:"sequence"`
	Sender           ConversationParticipant   `json:"sender"`
	Content          string                    `json:"content"`
	OffsetBytes      int                       `json:"offsetBytes"`
	NextOffsetBytes  *int                      `json:"nextOffsetBytes,omitempty"`
	References       []ConversationReference   `json:"references,omitempty"`
}

type ConversationHistoryReadResult struct {
	Messages           []ConversationHistoryExcerpt `json:"messages"`
	NextBeforeSequence int64                        `json:"nextBeforeSequence,omitempty"`
}

// ReadConversationHistory returns bounded excerpts of durable originals, not a
// synthesized recollection. Paging offsets are UTF-8 byte boundaries. Hidden
// messages and their identifiers are never included in cursors or results.
func ReadConversationHistory(ctx context.Context, store ConversationHistoryReadStore, scope Scope, owner ObjectiveOwner, conversationID string, viewer ConversationViewer, request ConversationHistoryReadRequest) (*ConversationHistoryReadResult, error) {
	denied := errors.New("conversation history is unavailable to this run")
	if store == nil || scope.Validate() != nil || owner.Validate() != nil || viewer.Validate() != nil || conversationID == "" {
		return nil, denied
	}
	if request.Limit < 0 || request.Limit > 10 || request.BeforeSequence < 0 || request.OffsetBytes < 0 || request.MessageID == "" && request.OffsetBytes != 0 || request.MessageID != "" && request.BeforeSequence != 0 {
		return nil, errors.New("invalid history page")
	}
	conversation, err := store.GetConversation(ctx, scope, conversationID)
	if err != nil || conversation == nil || conversation.ID != conversationID || conversation.Scope != scope || conversation.Owner != owner {
		return nil, denied
	}
	var messages []*ChannelMessage
	limit := request.Limit
	if limit == 0 {
		limit = 5
	}
	if request.MessageID != "" {
		message, err := store.GetVisibleChannelMessage(ctx, scope, conversationID, request.MessageID, viewer)
		if err != nil || message == nil {
			return nil, denied
		}
		messages = []*ChannelMessage{message}
	} else {
		messages, err = store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversationID, Viewer: &viewer, BeforeSequence: request.BeforeSequence, Descending: true, Limit: limit + 1})
		if err != nil {
			return nil, denied
		}
	}
	result := &ConversationHistoryReadResult{Messages: make([]ConversationHistoryExcerpt, 0)}
	if len(messages) > limit {
		messages = messages[:limit]
		if messages[len(messages)-1] == nil {
			return nil, denied
		}
		result.NextBeforeSequence = messages[len(messages)-1].Sequence
	}
	for _, message := range messages {
		if message == nil || message.Scope != scope || message.ConversationID != conversationID || !CanViewChannelMessage(message, viewer) {
			return nil, denied
		}
		start := request.OffsetBytes
		if start > len(message.Content) || start < len(message.Content) && !utf8.RuneStart(message.Content[start]) {
			return nil, errors.New("invalid history byte offset")
		}
		end := min(len(message.Content), start+4096)
		for end < len(message.Content) && !utf8.RuneStart(message.Content[end]) {
			end--
		}
		excerpt := ConversationHistoryExcerpt{CreatedAt: message.CreatedAt, Intent: message.Intent, ReplyToMessageID: message.ReplyToMessageID, ID: message.ID, Sequence: message.Sequence, Sender: message.Sender, Content: message.Content[start:end], OffsetBytes: start, References: append([]ConversationReference(nil), message.References...)}
		if end < len(message.Content) {
			next := end
			excerpt.NextOffsetBytes = &next
		}
		result.Messages = append(result.Messages, excerpt)
	}
	return result, nil
}
