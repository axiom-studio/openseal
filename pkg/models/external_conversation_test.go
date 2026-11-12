package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestNormalizedExternalConversationReplyParentIsOptionalAndValidated(t *testing.T) {
	event := NormalizedExternalConversationEvent{
		ID: "event", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "chat", ExternalMessageID: "chat:2", ExternalParticipantID: "person", Text: "hello",
		OrderingKey: "chat:reply:1", OccurredAt: time.Now().UTC(),
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("existing event contract rejected: %v", err)
	}
	event.ReplyToExternalMessageID = "chat:1"
	encoded, err := json.Marshal(event)
	if err != nil || !strings.Contains(string(encoded), `"replyToExternalMessageId":"chat:1"`) {
		t.Fatalf("portable reply parent = %s, %v", encoded, err)
	}
	var decoded NormalizedExternalConversationEvent
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.ReplyToExternalMessageID != event.ReplyToExternalMessageID || decoded.Validate() != nil {
		t.Fatalf("reply parent round trip = %#v, %v", decoded, err)
	}
	for _, invalid := range []string{" chat:1", "chat:1\n", strings.Repeat("x", 1025)} {
		event.ReplyToExternalMessageID = invalid
		if err := event.Validate(); err == nil {
			t.Fatalf("invalid reply parent %q was accepted", invalid)
		}
	}
}
