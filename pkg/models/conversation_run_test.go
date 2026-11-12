package models

import (
	"testing"
)

func TestTelegramPrivateChatKeepsContextAcrossReplyRoots(t *testing.T) {
	conversation := &Conversation{Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "telegram-endpoint"}}
	trigger := &ChannelMessage{ID: "new-message", ThreadRootID: "quoted-message", ExternalSource: &ExternalMessageSource{Provider: "telegram", ChannelType: "private", ThreadID: "reply:7"}}
	if got := ExternalConversationThreadRoot(conversation, trigger); got != "" {
		t.Fatalf("DM context restricted to %s", got)
	}
	for _, topic := range []string{"topic:7", "direct-topic:7"} {
		trigger.ExternalSource.ThreadID = topic
		if got := ExternalConversationThreadRoot(conversation, trigger); got != "quoted-message" {
			t.Fatalf("topic lost isolation: %s", got)
		}
	}
	trigger.ExternalSource.ThreadID = "reply:7"
	trigger.ExternalSource.ChannelType = "supergroup"
	if ExternalConversationThreadRoot(conversation, trigger) != "quoted-message" {
		t.Fatal("group lost isolation")
	}
	trigger.ExternalSource.Provider = "slack"
	trigger.ExternalSource.ChannelType = "private"
	if ExternalConversationThreadRoot(conversation, trigger) != "quoted-message" {
		t.Fatal("Slack lost isolation")
	}
}
