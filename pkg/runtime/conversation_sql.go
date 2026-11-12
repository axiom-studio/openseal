package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeConversation(payload string) (*models.Conversation, error) {
	var value models.Conversation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation: %w", err)
	}
	models.ApplyConversationDefaults(&value)
	return &value, nil
}

func decodeChannelMessage(payload string) (*models.ChannelMessage, error) {
	var value models.ChannelMessage
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode channel message: %w", err)
	}
	return &value, nil
}

func decodeParticipationRound(payload string) (*models.ParticipationRound, error) {
	var value models.ParticipationRound
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode participation round: %w", err)
	}
	return &value, nil
}

func decodeConversationCursor(payload string) (*models.ConversationCursor, error) {
	var value models.ConversationCursor
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation cursor: %w", err)
	}
	return &value, nil
}

func decodeConversationPresence(payload string) (*models.ConversationPresence, error) {
	var value models.ConversationPresence
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation presence: %w", err)
	}
	return &value, nil
}
