package models

import (
	"context"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	// ConversationReactionAdded and ConversationReactionRemoved are the
	// change-stream event types of a reaction change. They are the same names
	// external adapters use for inbound provider reactions.
	ConversationReactionAdded   = capability.ConversationEventReactionAdded
	ConversationReactionRemoved = capability.ConversationEventReactionRemoved
	// MaximumReactionEmojiBytes bounds one reaction emoji.
	MaximumReactionEmojiBytes = 16
)

// MessageReaction is one emoji on a channel message and everyone who reacted
// with it, in reaction order. It is filled on read and never persisted inside
// the message.
type MessageReaction struct {
	Emoji  string                    `json:"emoji"`
	Actors []ConversationParticipant `json:"actors"`
}

// MessageReactionRecord is one stored reaction: one actor's emoji on one
// message. (message, actor, emoji) is unique.
type MessageReactionRecord struct {
	Scope          Scope                   `json:"scope"`
	ConversationID string                  `json:"conversationId"`
	MessageID      string                  `json:"messageId"`
	Actor          ConversationParticipant `json:"actor"`
	Emoji          string                  `json:"emoji"`
	CreatedAt      time.Time               `json:"createdAt"`
}

// ConversationReactionEvent is a durable reaction change. Events of one
// conversation are numbered by Sequence in commit order, so the change stream
// delivers them as deltas like messages. Reactions is the message's current
// reactions when the event is read, so a client can replace them outright.
type ConversationReactionEvent struct {
	Sequence       int64                   `json:"sequence"`
	Type           string                  `json:"type"`
	Scope          Scope                   `json:"scope"`
	ConversationID string                  `json:"conversationId"`
	MessageID      string                  `json:"messageId"`
	Emoji          string                  `json:"emoji"`
	Actor          ConversationParticipant `json:"actor"`
	CreatedAt      time.Time               `json:"createdAt"`
	Reactions      []MessageReaction       `json:"reactions"`
}

// ConversationReactionStore keeps message reactions and their change log.
// Add and Remove change the reaction and append its event atomically; they
// return a nil event when nothing changed.
type ConversationReactionStore interface {
	AddMessageReaction(ctx context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error)
	RemoveMessageReaction(ctx context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error)
	// ListMessageReactions returns the reactions on the given messages,
	// oldest first.
	ListMessageReactions(ctx context.Context, scope Scope, conversationID string, messageIDs []string) ([]*MessageReactionRecord, error)
	ListConversationReactionEvents(ctx context.Context, scope Scope, conversationID string, afterSequence int64, limit int) ([]*ConversationReactionEvent, error)
}

// ValidateReactionEmoji accepts one emoji-like grapheme: at most 16 bytes of
// valid UTF-8, no whitespace or control characters, at least one non-ASCII
// rune, and ASCII only for keycap bases (0-9, # and *).
func ValidateReactionEmoji(emoji string) error {
	if emoji == "" || len(emoji) > MaximumReactionEmojiBytes || !utf8.ValidString(emoji) {
		return fmt.Errorf("%w: reaction emoji must be 1-%d bytes of UTF-8", ErrInvalidConversation, MaximumReactionEmojiBytes)
	}
	nonASCII := false
	for _, r := range emoji {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError:
			return fmt.Errorf("%w: reaction emoji cannot contain whitespace or control characters", ErrInvalidConversation)
		case r >= utf8.RuneSelf:
			nonASCII = true
		case !(r >= '0' && r <= '9') && r != '#' && r != '*':
			return fmt.Errorf("%w: reaction must be a single emoji", ErrInvalidConversation)
		}
	}
	if !nonASCII {
		return fmt.Errorf("%w: reaction must be a single emoji", ErrInvalidConversation)
	}
	return nil
}

func (r MessageReactionRecord) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(r.ConversationID, 256) || !IsValidOpaqueIdentifier(r.MessageID, 256) {
		return fmt.Errorf("%w: reaction requires a conversation and message", ErrInvalidConversation)
	}
	if err := r.Actor.Validate(); err != nil {
		return err
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("%w: reaction time is required", ErrInvalidConversation)
	}
	return ValidateReactionEmoji(r.Emoji)
}

func NewConversationReactionEvent(eventType string, record MessageReactionRecord, sequence int64) *ConversationReactionEvent {
	return &ConversationReactionEvent{
		Sequence: sequence, Type: eventType, Scope: record.Scope, ConversationID: record.ConversationID,
		MessageID: record.MessageID, Emoji: record.Emoji, Actor: record.Actor, CreatedAt: record.CreatedAt,
	}
}

func CloneConversationReactionEvent(in *ConversationReactionEvent) *ConversationReactionEvent {
	if in == nil {
		return nil
	}
	out := *in
	out.Reactions = CloneMessageReactions(in.Reactions)
	return &out
}

func CloneMessageReactions(in []MessageReaction) []MessageReaction {
	if in == nil {
		return nil
	}
	out := make([]MessageReaction, len(in))
	for index, reaction := range in {
		out[index] = MessageReaction{Emoji: reaction.Emoji, Actors: CloneConversationParticipants(reaction.Actors)}
	}
	return out
}

// PersistedChannelMessage is the message as stored: reactions live in their
// own table and never in the message payload.
func PersistedChannelMessage(message *ChannelMessage) *ChannelMessage {
	if message == nil || message.Reactions == nil {
		return message
	}
	stored := CloneChannelMessage(message)
	stored.Reactions = nil
	return stored
}
