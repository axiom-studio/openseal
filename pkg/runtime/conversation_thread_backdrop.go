package runtime

import (
	"context"
	"sort"
	"unicode/utf8"
)

const maximumConversationThreadBackdropMessages = 8
const maximumConversationThreadBackdropContentBytes = 1024

// A thread gets a stable, authorized view of the discussion it continued.
// These excerpts are prompt-only context, not entries in its transcript or
// additional authority for the scoped history tool.
func loadConversationThreadBackdrop(ctx context.Context, service *ConversationService, conversation *Conversation, trigger *ChannelMessage, viewer ConversationViewer) ([]*ChannelMessage, error) {
	if service == nil || conversation == nil || trigger == nil || !trigger.StartsThread && trigger.ThreadRootID == "" {
		return nil, nil
	}
	rootID := externalConversationThreadRoot(conversation, trigger)
	if rootID == "" {
		return nil, nil
	}
	root, err := service.GetVisibleChannelMessage(ctx, conversation.Scope, conversation.ID, rootID, viewer)
	if err != nil {
		return nil, err
	}
	if !root.StartsThread {
		return nil, nil
	}
	// A shared agent can see messages that this caller cannot. Derive the
	// caller only from the canonical root and intersect both viewers; agent
	// access alone must never disclose another person's private discussion.
	actor := root.Sender
	if actor.Type == ConversationParticipantService && root.Initiator != nil {
		actor = *root.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil {
		return nil, nil
	}
	caller := ConversationViewer{Participant: actor}
	filter := ChannelMessageFilter{
		Scope: conversation.Scope, ConversationID: conversation.ID,
		BeforeSequence: root.Sequence, Descending: true, Limit: maximumConversationThreadBackdropMessages * 2,
	}
	var parent *ChannelMessage
	if root.ReplyToMessageID != "" {
		parent, err = service.GetVisibleChannelMessage(ctx, conversation.Scope, conversation.ID, root.ReplyToMessageID, viewer)
		if err != nil {
			return nil, err
		}
		filter.ThreadRootID = parent.ThreadRootID
		if filter.ThreadRootID == "" {
			filter.ThreadRootID = parent.ID
		}
	} else {
		filter.ChannelTimeline = true
	}
	// Read one candidate page rather than filling through hidden history. A
	// call backdrop may contain fewer excerpts; it never scans arbitrary older
	// pages merely to fill its prompt allowance.
	prior, err := service.store.ListChannelMessages(ctx, filter)
	if err != nil {
		return nil, err
	}
	prior, err = service.filterVisibleChannelMessages(ctx, filter.Scope, filter.ConversationID, prior, viewer)
	if err != nil {
		return nil, err
	}
	parentID := ""
	if parent != nil {
		parentID = parent.ID
		prior = append(prior, parent)
	}
	prior, err = service.filterVisibleChannelMessages(ctx, filter.Scope, filter.ConversationID, prior, caller)
	if err != nil {
		return nil, err
	}
	parent = nil
	for _, message := range prior {
		if message.ID == parentID {
			parent = message
			break
		}
	}
	selected := make([]*ChannelMessage, 0, maximumConversationThreadBackdropMessages)
	seen := make(map[string]bool)
	add := func(message *ChannelMessage, exactParent bool) {
		if message == nil || seen[message.ID] || message.Sequence >= root.Sequence || message.Scope != conversation.Scope || message.ConversationID != conversation.ID || message.StartsThread && !exactParent || len(selected) == maximumConversationThreadBackdropMessages {
			return
		}
		seen[message.ID] = true
		copy := cloneChannelMessage(message)
		copy.Historical = true
		if len(copy.Content) > maximumConversationThreadBackdropContentBytes {
			end := maximumConversationThreadBackdropContentBytes
			for end > 0 && !utf8.RuneStart(copy.Content[end]) {
				end--
			}
			copy.Content = copy.Content[:end]
		}
		selected = append(selected, copy)
	}
	// Always retain the exact selected parent, even when a long parent thread
	// has pushed its root outside the most recent page.
	add(parent, true)
	for _, message := range prior {
		add(message, false)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Sequence < selected[j].Sequence })
	return selected, nil
}
