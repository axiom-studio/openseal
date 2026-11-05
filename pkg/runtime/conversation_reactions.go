package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// maximumReactionMessageBatch bounds one reaction read.
	maximumReactionMessageBatch = 500
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

// MessageReactionProjector mirrors a reaction change made in OpenSeal to the
// external conversation its message belongs to. Reactions mirrored in from an
// external conversation are never projected back out.
type MessageReactionProjector interface {
	ProjectMessageReaction(ctx context.Context, event *ConversationReactionEvent) error
}

// MessageReactionRequest adds or removes one actor's emoji on a message the
// actor can see.
type MessageReactionRequest struct {
	Scope          Scope
	ConversationID string
	MessageID      string
	Emoji          string
	Actor          ConversationParticipant
	// ActorRoles are the actor's audience roles, for message visibility.
	ActorRoles []string
	// FromExternal marks a reaction mirrored in from an external
	// conversation. It is not projected back out.
	FromExternal bool
}

type MessageReactionResult struct {
	Changed   bool                       `json:"changed"`
	Event     *ConversationReactionEvent `json:"event,omitempty"`
	Reactions []MessageReaction          `json:"reactions"`
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

func (r MessageReactionRecord) validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(r.ConversationID, 256) || !validOpaqueIdentifier(r.MessageID, 256) {
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

func newConversationReactionEvent(eventType string, record MessageReactionRecord, sequence int64) *ConversationReactionEvent {
	return &ConversationReactionEvent{
		Sequence: sequence, Type: eventType, Scope: record.Scope, ConversationID: record.ConversationID,
		MessageID: record.MessageID, Emoji: record.Emoji, Actor: record.Actor, CreatedAt: record.CreatedAt,
	}
}

func cloneConversationReactionEvent(in *ConversationReactionEvent) *ConversationReactionEvent {
	if in == nil {
		return nil
	}
	out := *in
	out.Reactions = cloneMessageReactions(in.Reactions)
	return &out
}

func cloneMessageReactions(in []MessageReaction) []MessageReaction {
	if in == nil {
		return nil
	}
	out := make([]MessageReaction, len(in))
	for index, reaction := range in {
		out[index] = MessageReaction{Emoji: reaction.Emoji, Actors: cloneParticipants(reaction.Actors)}
	}
	return out
}

// groupMessageReactions folds oldest-first records into per-message
// reactions, each emoji in order of its first use.
func groupMessageReactions(records []*MessageReactionRecord) map[string][]MessageReaction {
	sorted := append([]*MessageReactionRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	result := make(map[string][]MessageReaction)
	for _, record := range sorted {
		if record == nil {
			continue
		}
		reactions := result[record.MessageID]
		index := -1
		for candidate := range reactions {
			if reactions[candidate].Emoji == record.Emoji {
				index = candidate
				break
			}
		}
		if index < 0 {
			reactions = append(reactions, MessageReaction{Emoji: record.Emoji})
			index = len(reactions) - 1
		}
		reactions[index].Actors = append(reactions[index].Actors, record.Actor)
		result[record.MessageID] = reactions
	}
	return result
}

// loadMessageReactions reads the reactions of the given messages in batches.
func loadMessageReactions(ctx context.Context, store ConversationStore, scope Scope, conversationID string, messageIDs []string) (map[string][]MessageReaction, error) {
	unique := make([]string, 0, len(messageIDs))
	seen := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	records := make([]*MessageReactionRecord, 0)
	for start := 0; start < len(unique); start += maximumReactionMessageBatch {
		end := min(start+maximumReactionMessageBatch, len(unique))
		batch, err := store.ListMessageReactions(ctx, scope, conversationID, unique[start:end])
		if err != nil {
			return nil, err
		}
		records = append(records, batch...)
	}
	return groupMessageReactions(records), nil
}

// withMessageReactions returns the messages with their current reactions.
// Messages are cloned before they are changed. Messages from several
// conversations are read per conversation.
func withMessageReactions(ctx context.Context, store ConversationStore, messages []*ChannelMessage) ([]*ChannelMessage, error) {
	type conversationKey struct {
		scope Scope
		id    string
	}
	ids := make(map[conversationKey][]string)
	order := make([]conversationKey, 0, 1)
	for _, message := range messages {
		if message == nil {
			continue
		}
		key := conversationKey{scope: message.Scope, id: message.ConversationID}
		if _, known := ids[key]; !known {
			order = append(order, key)
		}
		ids[key] = append(ids[key], message.ID)
	}
	if len(order) == 0 {
		return messages, nil
	}
	reactions := make(map[conversationKey]map[string][]MessageReaction, len(order))
	for _, key := range order {
		loaded, err := loadMessageReactions(ctx, store, key.scope, key.id, ids[key])
		if err != nil {
			return nil, err
		}
		reactions[key] = loaded
	}
	out := make([]*ChannelMessage, len(messages))
	for index, message := range messages {
		out[index] = message
		if message == nil {
			continue
		}
		current := reactions[conversationKey{scope: message.Scope, id: message.ConversationID}][message.ID]
		if len(current) == 0 && len(message.Reactions) == 0 {
			continue
		}
		filled := cloneChannelMessage(message)
		filled.Reactions = cloneMessageReactions(current)
		out[index] = filled
	}
	return out, nil
}

// persistedChannelMessage is the message as stored: reactions live in their
// own table and never in the message payload.
func persistedChannelMessage(message *ChannelMessage) *ChannelMessage {
	if message == nil || message.Reactions == nil {
		return message
	}
	stored := cloneChannelMessage(message)
	stored.Reactions = nil
	return stored
}

// SetReactionProjector mirrors later reaction changes made through this
// service to external conversations.
func (s *ConversationService) SetReactionProjector(projector MessageReactionProjector) {
	if s != nil {
		s.reactions = projector
	}
}

// AddMessageReaction adds the actor's emoji to a message the actor can see.
// Adding a present reaction is not an error; Changed reports whether it was
// new.
func (s *ConversationService) AddMessageReaction(ctx context.Context, req MessageReactionRequest) (*MessageReactionResult, error) {
	return s.changeMessageReaction(ctx, req, true)
}

// RemoveMessageReaction removes the actor's emoji from a message the actor
// can see. Removing an absent reaction is not an error.
func (s *ConversationService) RemoveMessageReaction(ctx context.Context, req MessageReactionRequest) (*MessageReactionResult, error) {
	return s.changeMessageReaction(ctx, req, false)
}

func (s *ConversationService) changeMessageReaction(ctx context.Context, req MessageReactionRequest, add bool) (*MessageReactionResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	record := MessageReactionRecord{
		Scope: req.Scope, ConversationID: strings.TrimSpace(req.ConversationID), MessageID: strings.TrimSpace(req.MessageID),
		Actor: req.Actor, Emoji: req.Emoji, CreatedAt: s.now().UTC(),
	}
	if err := record.validate(); err != nil {
		return nil, err
	}
	viewer := ConversationViewer{Participant: req.Actor, Roles: append([]string(nil), req.ActorRoles...)}
	if _, err := s.GetVisibleChannelMessage(ctx, record.Scope, record.ConversationID, record.MessageID, viewer); err != nil {
		return nil, err
	}
	var event *ConversationReactionEvent
	var err error
	if add {
		event, err = s.store.AddMessageReaction(ctx, record)
	} else {
		event, err = s.store.RemoveMessageReaction(ctx, record)
	}
	if err != nil {
		return nil, err
	}
	reactions, err := loadMessageReactions(ctx, s.store, record.Scope, record.ConversationID, []string{record.MessageID})
	if err != nil {
		return nil, err
	}
	result := &MessageReactionResult{Changed: event != nil, Reactions: reactions[record.MessageID]}
	if result.Reactions == nil {
		result.Reactions = []MessageReaction{}
	}
	if event != nil {
		event.Reactions = cloneMessageReactions(result.Reactions)
		result.Event = event
		// The reaction is durable; mirroring it is best effort through the
		// durable delivery outbox and never undoes it.
		if s.reactions != nil && !req.FromExternal {
			_ = s.reactions.ProjectMessageReaction(ctx, cloneConversationReactionEvent(event))
		}
	}
	return result, nil
}

// visibleReactionEvents keeps the events on messages the viewer can see and
// fills each with its message's current reactions.
func (s *ConversationService) visibleReactionEvents(ctx context.Context, scope Scope, conversationID string, events []*ConversationReactionEvent, viewer *ConversationViewer) ([]*ConversationReactionEvent, error) {
	if len(events) == 0 {
		return []*ConversationReactionEvent{}, nil
	}
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.MessageID)
	}
	visible := make(map[string]bool, len(ids))
	if viewer == nil {
		for _, id := range ids {
			visible[id] = true
		}
	} else {
		var messages []*ChannelMessage
		if batch, ok := s.store.(ChannelMessageBatchStore); ok {
			loaded, err := loadChannelMessageBatches(ctx, batch, scope, conversationID, ids)
			if err != nil {
				return nil, err
			}
			for _, message := range loaded {
				messages = append(messages, message)
			}
		} else {
			seen := make(map[string]bool, len(ids))
			for _, id := range ids {
				if seen[id] {
					continue
				}
				seen[id] = true
				message, err := s.store.GetChannelMessage(ctx, scope, conversationID, id)
				if err != nil {
					return nil, err
				}
				messages = append(messages, message)
			}
		}
		present := messages[:0]
		for _, message := range messages {
			if message != nil {
				present = append(present, message)
			}
		}
		allowed, err := s.filterVisibleChannelMessages(ctx, scope, conversationID, present, *viewer)
		if err != nil {
			return nil, err
		}
		for _, message := range allowed {
			visible[message.ID] = true
		}
	}
	reactions, err := loadMessageReactions(ctx, s.store, scope, conversationID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*ConversationReactionEvent, 0, len(events))
	for _, event := range events {
		if !visible[event.MessageID] {
			continue
		}
		filled := cloneConversationReactionEvent(event)
		filled.Reactions = cloneMessageReactions(reactions[event.MessageID])
		if filled.Reactions == nil {
			filled.Reactions = []MessageReaction{}
		}
		out = append(out, filled)
	}
	return out, nil
}

// reactionPromptLines renders each message's reactions for an Agent's
// conversation context, e.g. "👍 Rio, ✅ Maya".
func reactionPromptLines(messages []*ChannelMessage, extra ...*ChannelMessage) map[string]string {
	names := map[ConversationParticipant]string{}
	all := append(append([]*ChannelMessage(nil), messages...), extra...)
	for _, message := range all {
		if message != nil && message.SenderDisplayName != "" {
			if _, known := names[message.Sender]; !known {
				names[message.Sender] = message.SenderDisplayName
			}
		}
	}
	lines := make(map[string]string)
	for _, message := range all {
		if message == nil || len(message.Reactions) == 0 {
			continue
		}
		parts := make([]string, 0)
		for _, reaction := range message.Reactions {
			for _, actor := range reaction.Actors {
				name := names[actor]
				if name == "" {
					name = actor.ID
					if actor.Type == ConversationParticipantUser {
						name = "a user"
					}
				}
				parts = append(parts, reaction.Emoji+" "+name)
			}
		}
		lines[message.ID] = strings.Join(parts, ", ")
	}
	return lines
}

// MemoryStore reaction storage.

func (s *MemoryStore) AddMessageReaction(_ context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error) {
	return s.changeMemoryReaction(record, true)
}

func (s *MemoryStore) RemoveMessageReaction(_ context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error) {
	return s.changeMemoryReaction(record, false)
}

func (s *MemoryStore) changeMemoryReaction(record MessageReactionRecord, add bool) (*ConversationReactionEvent, error) {
	if err := record.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationStoreKey(record.Scope, record.ConversationID)
	if s.conversations[key] == nil {
		return nil, ErrConversationNotFound
	}
	if s.messageReactions == nil {
		s.messageReactions = make(map[string][]*MessageReactionRecord)
		s.reactionEvents = make(map[string][]*ConversationReactionEvent)
	}
	stored := s.messageReactions[key]
	index := -1
	for candidate, existing := range stored {
		if existing.MessageID == record.MessageID && existing.Actor == record.Actor && existing.Emoji == record.Emoji {
			index = candidate
			break
		}
	}
	eventType := ConversationReactionAdded
	switch {
	case add && index >= 0, !add && index < 0:
		return nil, nil
	case add:
		added := record
		s.messageReactions[key] = append(stored, &added)
	default:
		eventType = ConversationReactionRemoved
		s.messageReactions[key] = append(stored[:index:index], stored[index+1:]...)
	}
	event := newConversationReactionEvent(eventType, record, int64(len(s.reactionEvents[key]))+1)
	s.reactionEvents[key] = append(s.reactionEvents[key], event)
	return cloneConversationReactionEvent(event), nil
}

func (s *MemoryStore) ListMessageReactions(_ context.Context, scope Scope, conversationID string, messageIDs []string) ([]*MessageReactionRecord, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		wanted[id] = true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*MessageReactionRecord, 0)
	for _, record := range s.messageReactions[conversationStoreKey(scope, conversationID)] {
		if wanted[record.MessageID] {
			found := *record
			result = append(result, &found)
		}
	}
	return result, nil
}

func (s *MemoryStore) ListConversationReactionEvents(_ context.Context, scope Scope, conversationID string, afterSequence int64, limit int) ([]*ConversationReactionEvent, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maximumConversationChangeLimit+1 {
		limit = defaultConversationChangeLimit
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.conversations[conversationStoreKey(scope, conversationID)] == nil {
		return nil, ErrConversationNotFound
	}
	result := make([]*ConversationReactionEvent, 0)
	for _, event := range s.reactionEvents[conversationStoreKey(scope, conversationID)] {
		if len(result) == limit {
			break
		}
		if event.Sequence > afterSequence {
			result = append(result, cloneConversationReactionEvent(event))
		}
	}
	return result, nil
}
