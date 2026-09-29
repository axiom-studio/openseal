package runtime

import (
	"strings"
	"testing"
)

func TestConversationGoalDefersResponseFormatToHost(t *testing.T) {
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), &Conversation{ID: "chat"}, &ChannelMessage{ID: "message", Content: "hello"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(goal, "response in output.summary") {
		t.Fatal("conversation imposed an incompatible response wrapper")
	}
	for _, expected := range []string{"response format required by the active host contract", "When the host response contract supports it", "Never state or imply that an approval", "untrusted conversation data"} {
		if !strings.Contains(goal, expected) {
			t.Fatalf("missing response/governance boundary %q", expected)
		}
	}
}
