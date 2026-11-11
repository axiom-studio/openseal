package runtime

import (
	"reflect"
	"slices"
	"time"
)

func validateChannelMessageCommitRecord(record ChannelMessageCommitRecord) error {
	if record.Conversation == nil || record.Message == nil {
		return ErrInvalidConversation
	}
	if err := record.Conversation.Validate(); err != nil {
		return err
	}
	if err := record.Message.Validate(); err != nil {
		return err
	}
	if record.Conversation.Scope != record.Message.Scope || record.Conversation.ID != record.Message.ConversationID || record.ExpectedRevision <= 0 {
		return ErrInvalidConversation
	}
	return validateReplyRunGuard(record.RunGuard)
}

func validateParticipationRoundCommitRecord(record ParticipationRoundCommitRecord) error {
	if record.Conversation == nil || record.Round == nil || record.ExpectedRevision <= 0 {
		return ErrInvalidConversation
	}
	if err := record.Conversation.Validate(); err != nil {
		return err
	}
	if err := record.Round.Validate(); err != nil {
		return err
	}
	if record.Conversation.Scope != record.Round.Scope || record.Conversation.ID != record.Round.ConversationID {
		return ErrInvalidConversation
	}
	for _, message := range record.Messages {
		if err := message.Validate(); err != nil {
			return err
		}
		if message.Scope != record.Round.Scope || message.ConversationID != record.Round.ConversationID {
			return ErrInvalidConversation
		}
	}
	return nil
}

func sameChannelMessage(left, right *ChannelMessage) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftCopy, rightCopy := *left, *right
	leftCopy.Sequence, rightCopy.Sequence = 0, 0
	leftCopy.CreatedAt, rightCopy.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func sameParticipationRound(left, right *ParticipationRound) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.ID == right.ID && left.Scope == right.Scope && left.ConversationID == right.ConversationID &&
		left.TriggerMessageID == right.TriggerMessageID && left.IdempotencyKey == right.IdempotencyKey &&
		slices.Equal(left.Responders, right.Responders)
}

func cloneParticipationRoundResult(in *ParticipationRoundResult, replayed bool) *ParticipationRoundResult {
	if in == nil {
		return nil
	}
	return &ParticipationRoundResult{
		Conversation: cloneConversation(in.Conversation), Round: cloneParticipationRound(in.Round),
		Messages: cloneChannelMessages(in.Messages), Replayed: replayed,
	}
}
