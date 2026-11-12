package models

import (
	"errors"
	"strings"
	"time"
)

type ParticipationRoundStatus string

const (
	ParticipationRoundCommitted ParticipationRoundStatus = "committed"
)

// ConversationAddressing says why a Team member answers a channel message.
type ConversationAddressing string

const (
	// ConversationAddressedMention: the message @mentioned the member.
	ConversationAddressedMention ConversationAddressing = "mention"
	// ConversationAddressedEveryone: the message was sent to @everyone.
	ConversationAddressedEveryone ConversationAddressing = "everyone"
	// ConversationAddressedReply: the message quoted or replied to the member.
	ConversationAddressedReply ConversationAddressing = "reply"
	// ConversationAddressedRole: the message's audience named the member's role.
	ConversationAddressedRole ConversationAddressing = "role"
	// ConversationAddressedRouted: nobody was addressed; the router picked the member.
	ConversationAddressedRouted ConversationAddressing = "routed"
)

// MaximumRoutingReasonBytes bounds the short, user-facing routing reason.
const MaximumRoutingReasonBytes = 280

// ConversationResponder is one Team member asked to answer a channel message.
// Each responder answers in its own child agent_work Run (RunID).
type ConversationResponder struct {
	Participant ConversationParticipant `json:"participant"`
	Addressed   ConversationAddressing  `json:"addressed"`
	// Reason is the router's short, user-facing reason ("owns the press
	// list"). It is set only when the member was routed, not addressed.
	Reason string `json:"reason,omitempty"`
	RunID  string `json:"runId,omitempty"`
}

func (r ConversationResponder) Validate() error {
	if err := r.Participant.Validate(); err != nil {
		return err
	}
	if r.Participant.Type != ConversationParticipantAgent {
		return errors.New("conversation responders must be Agents")
	}
	switch r.Addressed {
	case ConversationAddressedMention, ConversationAddressedEveryone, ConversationAddressedReply, ConversationAddressedRole:
		if r.Reason != "" {
			return errors.New("only a routed responder carries a routing reason")
		}
	case ConversationAddressedRouted:
		if strings.TrimSpace(r.Reason) == "" || len(r.Reason) > MaximumRoutingReasonBytes {
			return errors.New("a routed responder requires a short routing reason")
		}
	default:
		return errors.New("conversation responder addressing is invalid")
	}
	if r.RunID != "" && !IsValidOpaqueIdentifier(r.RunID, 128) {
		return errors.New("conversation responder Run id must be portable")
	}
	return nil
}

// ParticipationRound is the durable routing decision for one channel message:
// who answers it and why. It holds no message content; each responder commits
// its own answer when its child Run finishes.
type ParticipationRound struct {
	ID                   string                   `json:"id"`
	Scope                Scope                    `json:"scope"`
	ConversationID       string                   `json:"conversationId"`
	ConversationRevision int64                    `json:"conversationRevision,omitempty"`
	TriggerMessageID     string                   `json:"triggerMessageId,omitempty"`
	Status               ParticipationRoundStatus `json:"status"`
	Responders           []ConversationResponder  `json:"responders"`
	IdempotencyKey       string                   `json:"idempotencyKey"`
	Revision             int64                    `json:"revision"`
	CreatedAt            time.Time                `json:"createdAt"`
	CommittedAt          time.Time                `json:"committedAt"`
}

// Validate accepts a round with no responders: a handoff or address that
// named nobody on the roster is still a decision.
func (r *ParticipationRound) Validate() error {
	if r == nil || !IsValidOpaqueIdentifier(r.ID, 128) || !IsValidOpaqueIdentifier(r.ConversationID, 128) || r.ConversationRevision < 0 ||
		r.Status != ParticipationRoundCommitted || r.Revision <= 0 || r.CreatedAt.IsZero() || r.CommittedAt.IsZero() ||
		strings.TrimSpace(r.IdempotencyKey) == "" || len(r.IdempotencyKey) > 256 {
		return errors.New("invalid participation round")
	}
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if r.TriggerMessageID != "" && !IsValidOpaqueIdentifier(r.TriggerMessageID, 128) {
		return errors.New("participation trigger message id must be portable")
	}
	seen := make(map[ConversationParticipant]bool, len(r.Responders))
	for _, responder := range r.Responders {
		if err := responder.Validate(); err != nil {
			return err
		}
		if seen[responder.Participant] {
			return errors.New("a participation round names a responder twice")
		}
		seen[responder.Participant] = true
	}
	return nil
}

func CloneParticipationRound(in *ParticipationRound) *ParticipationRound {
	if in == nil {
		return nil
	}
	out := *in
	out.Responders = append([]ConversationResponder(nil), in.Responders...)
	return &out
}
