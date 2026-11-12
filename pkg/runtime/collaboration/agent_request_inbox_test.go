package collaboration

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
)

func TestAgentRequestInboxBoundsConversationProjectionCadence(t *testing.T) {
	reconciler, err := NewAgentRequestInboxReconciler(conversationtest.NewStore(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	if !reconciler.conversationProjectionDue(now) {
		t.Fatal("initial conversation projection was not due")
	}
	if reconciler.conversationProjectionDue(now.Add(agentRequestConversationProjectionInterval - time.Nanosecond)) {
		t.Fatal("conversation projection ignored its minimum interval")
	}
	if !reconciler.conversationProjectionDue(now.Add(agentRequestConversationProjectionInterval)) {
		t.Fatal("conversation projection did not become due")
	}
}

func TestParseAgentRequestDecisionOutputRejectsAmbiguousOrUnsafeShape(t *testing.T) {
	for _, output := range []map[string]interface{}{
		nil,
		{AgentRequestDecisionOutputKey: map[string]interface{}{"decision": "maybe", "message": "unsure"}},
		{AgentRequestDecisionOutputKey: map[string]interface{}{"decision": string(AgentRequestDecisionRequestClarification)}},
	} {
		if _, _, err := ParseAgentRequestDecisionOutput(output); err == nil {
			t.Fatalf("output unexpectedly accepted: %#v", output)
		}
	}
}
