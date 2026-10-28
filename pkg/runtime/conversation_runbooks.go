package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MaximumConversationRunbooks bounds the routines linked to one
// conversation; linking more drops the oldest links.
const MaximumConversationRunbooks = 32

// maximumConversationRunbookLinkAttempts bounds retries when the
// conversation changes while a link is recorded.
const maximumConversationRunbookLinkAttempts = 5

// LinkRunbooks records that routine visuals in a conversation show these
// routines (runbook activation ids), so the conversation's change stream
// carries their live state. The link lives on the conversation itself
// (Conversation.RunbookActivationIDs), so every store keeps it without a
// schema change. Ids already linked keep their place; new ones are added
// last and, past MaximumConversationRunbooks, the oldest links drop. Hosts
// call it after a visual with routine components (presentation.
// RoutineBindings) passes validation and is stored, e.g. from update_visual;
// answer visuals are linked by the conversation runner.
func (s *ConversationService) LinkRunbooks(ctx context.Context, scope Scope, conversationID string, activationIDs []string) (*Conversation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	for _, id := range activationIDs {
		if !validOpaqueIdentifier(id, 128) || strings.TrimSpace(id) != id {
			return nil, fmt.Errorf("%w: routine id %q must be a portable identifier", ErrInvalidConversation, id)
		}
	}
	for attempt := 0; ; attempt++ {
		conversation, err := s.GetConversation(ctx, scope, conversationID)
		if err != nil {
			return nil, err
		}
		linked := linkRunbookIDs(conversation.RunbookActivationIDs, activationIDs)
		if linked == nil {
			return conversation, nil
		}
		updated := cloneConversation(conversation)
		updated.RunbookActivationIDs = linked
		updated.Revision++
		updated.UpdatedAt = s.now().UTC()
		result, err := s.store.UpdateConversation(ctx, updated, conversation.Revision)
		if errors.Is(err, ErrRevisionConflict) && attempt+1 < maximumConversationRunbookLinkAttempts {
			continue
		}
		return result, err
	}
}

// linkRunbookIDs adds ids to existing, keeping the newest
// MaximumConversationRunbooks. It returns nil when nothing changes.
func linkRunbookIDs(existing, ids []string) []string {
	linked := append([]string(nil), existing...)
	seen := make(map[string]bool, len(existing)+len(ids))
	for _, id := range existing {
		seen[id] = true
	}
	changed := false
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			linked = append(linked, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if extra := len(linked) - MaximumConversationRunbooks; extra > 0 {
		linked = linked[extra:]
	}
	return linked
}
