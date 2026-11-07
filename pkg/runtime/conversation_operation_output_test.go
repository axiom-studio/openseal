package runtime

import (
	"strings"
	"testing"
)

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
func TestReasoningDraftCompletionNeedsNoToolReceipt(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: "draft-turn", NextRunStatus: AgentRunStatusCompleted, ModelProvider: "test", Model: "writer", OutputSummary: "Draft ready", RunOutput: map[string]interface{}{"summary": "The requested short draft."}}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "writer", DefinitionID: "writer", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "draft", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Write a draft", Context: map[string]interface{}{DelegationModeContextKey: "reason"}}, Turn: &AgentTurn{ID: "draft-turn"}})
	if err != nil || outcome.NextRunStatus != AgentRunStatusCompleted || outcome.RunOutput["summary"] != "The requested short draft." {
		t.Fatalf("draft completion=%#v %v", outcome, err)
	}
	instructions := strings.Join(host.request.SystemInstructions, "\n")
	if !strings.Contains(instructions, "completed in that same response") || !strings.Contains(instructions, "completionEvidenceRefs must be empty") {
		t.Fatal("missing writing completion contract")
	}
	forged := *host.response
	forged.CompletionEvidenceRefs = []string{"draft"}
	if err := ValidateHostedTurnCompletion(host.request, &forged); err == nil {
		t.Fatal("accepted fabricated receipt")
	}
}
