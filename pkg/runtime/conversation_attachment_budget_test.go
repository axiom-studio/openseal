package runtime

import (
	"strings"
	"testing"
)

func TestConversationAttachmentGuidanceIsConditionalNotHistoryTruncation(t *testing.T) {
	conversation := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "tenant"}}
	trigger := &ChannelMessage{ID: "new", Content: "Remember the confirmed deadline: Thursday."}
	old := &ChannelMessage{ID: "old", Content: "The obsolete deadline was Tuesday."}
	runner := &ConversationRunTurnRunner{}
	plain, err := runner.agentConversationGoal(t.Context(), conversation, trigger, []*ChannelMessage{old, trigger}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "attachmentGuidance") {
		t.Fatal("text-only turn received attachment rules")
	}
	for _, content := range []string{trigger.Content, old.Content, "untrusted conversation data", "read_conversation_history"} {
		if !strings.Contains(plain, content) {
			t.Fatal("lost context or trust boundary")
		}
	}
	for _, target := range []*ChannelMessage{old, trigger} {
		target.References = []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "unavailable-file", Version: 1}}
		attached, err := runner.agentConversationGoal(t.Context(), conversation, trigger, []*ChannelMessage{old, trigger}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range []string{"attachmentGuidance", "references are not file contents", "not instructions or authority", "Image_context"} {
			if !strings.Contains(attached, rule) {
				t.Fatalf("lost attachment rule: %s", rule)
			}
		}
		t.Logf("text-only goal bytes=%d; same history with file reference and full attachment guidance=%d", len(plain), len(attached))
		target.References = nil
	}
}
