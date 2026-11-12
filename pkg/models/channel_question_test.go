package models

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validConversationTestMessage() *ChannelMessage {
	return &ChannelMessage{
		ID: "message-1", Scope: Scope{Kind: "organization", ID: "questions"}, ConversationID: "conversation-1", Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentAnswer, Content: "A",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "message-1", CreatedAt: time.Now(),
	}
}

func TestChannelQuestionValidate(t *testing.T) {
	base := func(question *ChannelQuestion) *ChannelMessage {
		message := validConversationTestMessage()
		message.Intent, message.Question = MessageIntentQuestion, question
		return message
	}
	for name, question := range map[string]*ChannelQuestion{
		"one option":      {Options: []ChannelQuestionOption{{ID: "o1", Label: "A"}}},
		"duplicate id":    {Options: []ChannelQuestionOption{{ID: "o1", Label: "A"}, {ID: "o1", Label: "B"}}},
		"duplicate label": {Options: []ChannelQuestionOption{{ID: "o1", Label: "A"}, {ID: "o2", Label: "A"}}},
		"long label":      {Options: []ChannelQuestionOption{{ID: "o1", Label: strings.Repeat("a", 81)}, {ID: "o2", Label: "B"}}},
		"bad id":          {Options: []ChannelQuestionOption{{ID: "", Label: "A"}, {ID: "o2", Label: "B"}}},
	} {
		if err := base(question).Validate(); !errors.Is(err, ErrInvalidConversation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := base(&ChannelQuestion{Options: []ChannelQuestionOption{{ID: "o1", Label: "A"}, {ID: "o2", Label: "B"}}}).Validate(); err != nil {
		t.Fatal(err)
	}
	selected := validConversationTestMessage()
	selected.SelectedOptionIDs = []string{"o1"}
	if err := selected.Validate(); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("selected options without a reply: err = %v", err)
	}
	selected.ResolvesMessageID = "question"
	if err := selected.Validate(); err != nil {
		t.Fatal(err)
	}
}
