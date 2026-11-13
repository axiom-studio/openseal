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

// MessageActor returns the participant a message acts for: its sender, or,
// for a service sender (a voice call starting, a finished sign-in), the
// participant that initiated it.
func MessageActor(message *ChannelMessage) ConversationParticipant {
	if message.Sender.Type == ConversationParticipantService && message.Initiator != nil {
		return *message.Initiator
	}
	return message.Sender
}

// MessageActingUser returns the human a conversation message acts for: its
// user sender, or, for a service sender, the user that initiated it. It
// returns "" when the message belongs to another conversation or no person
// sent it.
func MessageActingUser(conversation *Conversation, message *ChannelMessage) string {
	if conversation == nil || message == nil || message.Scope != conversation.Scope || message.ConversationID != conversation.ID {
		return ""
	}
	actor := MessageActor(message)
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil || !IsValidActingUser(actor.ID) {
		return ""
	}
	return actor.ID
}
