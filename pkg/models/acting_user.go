package models

import (
	"strings"
	"unicode"
)

// IsValidActingUser reports whether value may be stored as a run's, lease's or
// card's acting human: empty (no person), or a bounded opaque identifier with
// no whitespace.
func IsValidActingUser(value string) bool {
	if value == "" {
		return true
	}
	return IsValidOpaqueIdentifier(value, 256) && !strings.ContainsFunc(value, unicode.IsSpace)
}

// MessageActingUser returns the human a conversation message acts for: its
// user sender, or, for a service sender, the user that initiated it. It
// returns "" when the message belongs to another conversation or no person
// sent it.
func MessageActingUser(conversation *Conversation, message *ChannelMessage) string {
	if conversation == nil || message == nil || message.Scope != conversation.Scope || message.ConversationID != conversation.ID {
		return ""
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil || !IsValidActingUser(actor.ID) {
		return ""
	}
	return actor.ID
}
