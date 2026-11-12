package conversation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode"

	"github.com/axiom-studio/openseal/pkg/models"
)

const (
	// maximumReactionMessageBatch bounds one reaction read.
	maximumReactionMessageBatch = 500
)

// MessageReactionProjector mirrors a reaction change made in OpenSeal to the
// external conversation its message belongs to. Reactions mirrored in from an
// external conversation are never projected back out.
type MessageReactionProjector interface {
	ProjectMessageReaction(ctx context.Context, event *models.ConversationReactionEvent) error
}

// MessageReactionRequest adds or removes one actor's emoji on a message the
// actor can see.
type MessageReactionRequest struct {
	Scope          models.Scope
	ConversationID string
	MessageID      string
	Emoji          string
	Actor          models.ConversationParticipant
	// ActorRoles are the actor's audience roles, for message visibility.
	ActorRoles []string
	// FromExternal marks a reaction mirrored in from an external
	// conversation. It is not projected back out.
	FromExternal bool
}

// MessageReactionResult reports whether the reaction changed and carries
// the message as the actor sees it, with its current reactions.
type MessageReactionResult struct {
	Message   *models.ChannelMessage            `json:"message"`
	Changed   bool                              `json:"changed"`
	Event     *models.ConversationReactionEvent `json:"event,omitempty"`
	Reactions []models.MessageReaction          `json:"reactions"`
}

// groupMessageReactions folds oldest-first records into per-message
// reactions, each emoji in order of its first use.
func groupMessageReactions(records []*models.MessageReactionRecord) map[string][]models.MessageReaction {
	sorted := append([]*models.MessageReactionRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	result := make(map[string][]models.MessageReaction)
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
			reactions = append(reactions, models.MessageReaction{Emoji: record.Emoji})
			index = len(reactions) - 1
		}
		reactions[index].Actors = append(reactions[index].Actors, record.Actor)
		result[record.MessageID] = reactions
	}
	return result
}

// loadMessageReactions reads the reactions of the given messages in batches.
func loadMessageReactions(ctx context.Context, store models.ConversationStore, scope models.Scope, conversationID string, messageIDs []string) (map[string][]models.MessageReaction, error) {
	unique := make([]string, 0, len(messageIDs))
	seen := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	records := make([]*models.MessageReactionRecord, 0)
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
func withMessageReactions(ctx context.Context, store models.ConversationStore, messages []*models.ChannelMessage) ([]*models.ChannelMessage, error) {
	type conversationKey struct {
		scope models.Scope
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
	reactions := make(map[conversationKey]map[string][]models.MessageReaction, len(order))
	for _, key := range order {
		loaded, err := loadMessageReactions(ctx, store, key.scope, key.id, ids[key])
		if err != nil {
			return nil, err
		}
		reactions[key] = loaded
	}
	out := make([]*models.ChannelMessage, len(messages))
	for index, message := range messages {
		out[index] = message
		if message == nil {
			continue
		}
		current := reactions[conversationKey{scope: message.Scope, id: message.ConversationID}][message.ID]
		if len(current) == 0 && len(message.Reactions) == 0 {
			continue
		}
		filled := models.CloneChannelMessage(message)
		filled.Reactions = models.CloneMessageReactions(current)
		out[index] = filled
	}
	return out, nil
}

// SetReactionProjector mirrors later reaction changes made through this
// service to external conversations.
func (s *Service) SetReactionProjector(projector MessageReactionProjector) {
	if s != nil {
		s.reactions = projector
	}
}

// AddMessageReaction adds the actor's emoji to a message the actor can see.
// Adding a present reaction is not an error; Changed reports whether it was
// new.
func (s *Service) AddMessageReaction(ctx context.Context, req MessageReactionRequest) (*MessageReactionResult, error) {
	return s.changeMessageReaction(ctx, req, true)
}

// RemoveMessageReaction removes the actor's emoji from a message the actor
// can see. Removing an absent reaction is not an error.
func (s *Service) RemoveMessageReaction(ctx context.Context, req MessageReactionRequest) (*MessageReactionResult, error) {
	return s.changeMessageReaction(ctx, req, false)
}

func (s *Service) changeMessageReaction(ctx context.Context, req MessageReactionRequest, add bool) (*MessageReactionResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	record := models.MessageReactionRecord{
		Scope: req.Scope, ConversationID: strings.TrimSpace(req.ConversationID), MessageID: strings.TrimSpace(req.MessageID),
		Actor: req.Actor, Emoji: req.Emoji, CreatedAt: s.now().UTC(),
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	viewer := models.ConversationViewer{Participant: req.Actor, Roles: append([]string(nil), req.ActorRoles...)}
	message, err := s.GetVisibleChannelMessage(ctx, record.Scope, record.ConversationID, record.MessageID, viewer)
	if err != nil {
		return nil, err
	}
	var event *models.ConversationReactionEvent
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
		result.Reactions = []models.MessageReaction{}
	}
	result.Message = models.CloneChannelMessage(message)
	result.Message.Reactions = models.CloneMessageReactions(reactions[record.MessageID])
	if event != nil {
		event.Reactions = models.CloneMessageReactions(result.Reactions)
		result.Event = event
		// The reaction is durable; mirroring it is best effort through the
		// durable delivery outbox and never undoes it.
		if s.reactions != nil && !req.FromExternal {
			_ = s.reactions.ProjectMessageReaction(ctx, models.CloneConversationReactionEvent(event))
		}
	}
	return result, nil
}

// visibleReactionEvents keeps the events on messages the viewer can see and
// fills each with its message's current reactions.
func (s *Service) visibleReactionEvents(ctx context.Context, scope models.Scope, conversationID string, events []*models.ConversationReactionEvent, viewer *models.ConversationViewer) ([]*models.ConversationReactionEvent, error) {
	if len(events) == 0 {
		return []*models.ConversationReactionEvent{}, nil
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
		var messages []*models.ChannelMessage
		if batch, ok := s.store.(models.ChannelMessageBatchStore); ok {
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
	out := make([]*models.ConversationReactionEvent, 0, len(events))
	for _, event := range events {
		if !visible[event.MessageID] {
			continue
		}
		filled := models.CloneConversationReactionEvent(event)
		filled.Reactions = models.CloneMessageReactions(reactions[event.MessageID])
		if filled.Reactions == nil {
			filled.Reactions = []models.MessageReaction{}
		}
		out = append(out, filled)
	}
	return out, nil
}

// reactionPromptLines renders each message's reactions for an Agent's
// conversation context, e.g. "👍 Rio, ✅ Maya".
func reactionPromptLines(messages []*models.ChannelMessage, extra ...*models.ChannelMessage) map[string]string {
	names := map[models.ConversationParticipant]string{}
	all := append(append([]*models.ChannelMessage(nil), messages...), extra...)
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
					if actor.Type == models.ConversationParticipantUser {
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

// TurnReaction is the react action of a conversation turn: the answering
// Agent reacts to a message in the conversation it can see. A reaction is not
// a channel message; it never routes, schedules or answers anything.
type TurnReaction struct {
	MessageID string `json:"messageId"`
	Emoji     string `json:"emoji"`
}

// MaximumTurnReactions bounds the reactions one Run may add with react.
const MaximumTurnReactions = 3

// ReactInstruction is the description of the react control tool
// (control/control_interaction.go).
const ReactInstruction = "Add an emoji reaction as yourself. A reaction is not a reply and never asks anyone to act. When a message needs no real answer (thanks, ok, got it, a joke, good news), react instead of replying, such as 👍 ❤️ 😂 🎉, and finish with an empty answer."

func cloneTurnReactions(in []TurnReaction) []TurnReaction {
	return append([]TurnReaction(nil), in...)
}

// admitTurnReactions keeps the first valid, distinct reactions within the
// turn bound. Invalid reactions are dropped rather than costing the turn.
func admitTurnReactions(in []TurnReaction) []TurnReaction {
	var out []TurnReaction
	seen := make(map[TurnReaction]bool, len(in))
	for _, reaction := range in {
		reaction.MessageID = strings.TrimSpace(reaction.MessageID)
		if len(out) == MaximumTurnReactions || seen[reaction] ||
			!models.IsValidOpaqueIdentifier(reaction.MessageID, 256) || models.ValidateReactionEmoji(reaction.Emoji) != nil {
			continue
		}
		seen[reaction] = true
		out = append(out, reaction)
	}
	return out
}

// ReactionEcho reports whether content says nothing beyond the turn's
// reactions: empty, or only the reacted emoji and whitespace.
func ReactionEcho(content string, reactions []TurnReaction) bool {
	for _, reaction := range reactions {
		if reaction.Emoji != "" {
			content = strings.ReplaceAll(content, reaction.Emoji, "")
		}
	}
	return strings.TrimFunc(content, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFE0F' || r == '\u200D'
	}) == ""
}
