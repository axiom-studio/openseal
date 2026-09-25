package runtime

import (
	"strings"
	"testing"
)

func TestAgentConversationGoalPreservesAttachmentReferences(t *testing.T) {
	conversation := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "tenant"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}}
	message := &ChannelMessage{ID: "message", Content: "Can you read this?", References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "attachment-one", Version: 3}}}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, message, []*ChannelMessage{message}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"references":[{"kind":"artifact","id":"attachment-one","version":3}]`, `"attachmentGuidance":`, "references are not file contents", "Can you read this?"} {
		if !strings.Contains(goal, expected) {
			t.Fatalf("missing %q in projected conversation", expected)
		}
	}
	if message.References[0].ID != "attachment-one" {
		t.Fatal("input message was mutated")
	}
}

func TestAgentConversationGoalEndsWithCurrentRequest(t *testing.T) {
	conversation := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "tenant"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}}
	old := &ChannelMessage{ID: "old", Sequence: 1, Content: "Create a PDF"}
	current := &ChannelMessage{ID: "current", Sequence: 2, Content: "Reply with exactly READY."}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, current, []*ChannelMessage{old, current}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(goal, `"content":"Create a PDF"`) || !strings.HasSuffix(goal, `"content":"Reply with exactly READY."}}`) {
		t.Fatalf("historical request displaced the current request: %s", goal)
	}
}
