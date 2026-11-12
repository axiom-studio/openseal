package runtime

import (
	"encoding/json"
	"fmt"
)

func decodeConversation(payload string) (*Conversation, error) {
	var value Conversation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation: %w", err)
	}
	applyConversationDefaults(&value)
	return &value, nil
}

func decodeChannelMessage(payload string) (*ChannelMessage, error) {
	var value ChannelMessage
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode channel message: %w", err)
	}
	return &value, nil
}

func decodeParticipationRound(payload string) (*ParticipationRound, error) {
	var value ParticipationRound
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode participation round: %w", err)
	}
	return &value, nil
}

func decodeConversationCursor(payload string) (*ConversationCursor, error) {
	var value ConversationCursor
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation cursor: %w", err)
	}
	return &value, nil
}

func decodeConversationPresence(payload string) (*ConversationPresence, error) {
	var value ConversationPresence
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation presence: %w", err)
	}
	return &value, nil
}
