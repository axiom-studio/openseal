package runtime

import (
	"strings"
	"testing"
)

func TestConversationGoalOffersGovernedCapabilitySetup(t *testing.T) {
	conversation := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "one"}}
	trigger := &ChannelMessage{ID: "message", Content: "Send an hourly joke to my messaging service"}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, trigger, []*ChannelMessage{trigger}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`"capabilitySetupGuidance":`, "proactively offer to connect that specific service",
		"An empty installed registry is not proof that the marketplace is empty",
		"preserving its approval requirements", "Never fabricate an authorization URL",
		"do not promise automatic resumption otherwise", trigger.Content,
	} {
		if !strings.Contains(goal, expected) {
			t.Fatalf("missing setup instruction %q", expected)
		}
	}
}
