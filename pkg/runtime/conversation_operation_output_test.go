package runtime

import "testing"

func TestConversationOperationFailedHasNoSavedOutput(t *testing.T) {
	failed := &AgentRun{ID: "operation", Status: AgentRunStatusFailed, Output: map[string]interface{}{"summary": "partial draft"}}
	if output, err := conversationOperationSavedOutput(t.Context(), NewMemoryStore(), failed); err != nil || output != "" {
		t.Fatalf("failed operation output = %q, %v", output, err)
	}
}

func TestConversationOperationNeverDumpsArbitraryOutput(t *testing.T) {
	for _, output := range []map[string]interface{}{{"result": map[string]interface{}{"opaqueReference": "private"}}, {"raw": "private"}, {"result": []interface{}{map[string]interface{}{"summary": "unreviewed"}}}} {
		if got := conversationOperationSummary(output); got != "" {
			t.Fatalf("unrecognized output exposed: %s", got)
		}
	}
}
