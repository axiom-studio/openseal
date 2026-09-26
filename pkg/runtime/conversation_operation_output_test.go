package runtime

import (
	"strings"
	"testing"
	"time"
)

func TestConversationOperationDeliversDraftAndRecoversStatusOnlyReply(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "drafts"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "writer"}
			now := time.Now()
			conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "Writing", IdempotencyKey: "chat"})
			if err != nil {
				t.Fatal(err)
			}
			trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Write a short draft", "prompt")
			root := &AgentRun{ID: "root", Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, Goal: "Respond", Source: RunSourceChat, Status: AgentRunStatusCompleted, RootRunID: "root", Revision: 2, LastAppliedTurn: 1, CreatedAt: now, UpdatedAt: now, CompletedAt: &now, Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: trigger.ID}}
			operation := &AgentRun{ID: "operation", Scope: scope, Kind: RunKindAgentWork, Owner: owner, AssignedAgentID: owner.ID, Goal: "Write", Source: RunSourceFork, Status: status, RootRunID: "root", ParentRunID: "root", Entrypoint: "writing-session", Revision: 2, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}
			draft := "The actual requested draft, with all of its paragraphs."
			if status == AgentRunStatusCompleted {
				operation.Output = map[string]interface{}{"result": map[string]interface{}{"summary": draft}, "internal": map[string]interface{}{"opaqueReference": "private-value"}}
			}
			for _, run := range []*AgentRun{root, operation} {
				if err := store.CreateAgentRun(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			if status == AgentRunStatusFailed {
				leaf := cloneAgentRun(operation)
				leaf.ID = "writer-child"
				leaf.ParentRunID = operation.ID
				leaf.Entrypoint = ""
				leaf.LastAppliedTurn = 2
				leaf.Context = map[string]interface{}{DelegationModeContextKey: "reason"}
				leaf.Output = map[string]interface{}{"summary": draft}
				leaf.Checkpoint = map[string]interface{}{turnContinuityCheckpointKey: map[string]interface{}{"nextRunStatus": "running"}}
				leaf.Error = "provider returned multiple values"
				if err := store.CreateAgentRun(t.Context(), leaf); err != nil {
					t.Fatal(err)
				}
			}
			runner := &ConversationRunTurnRunner{portfolio: store, conversations: service}
			outcome, ok, err := runner.governedConversationOperationOutcome(t.Context(), root)
			if err != nil || !ok || !strings.Contains(outcome.Content, draft) || strings.Contains(outcome.Content, "private-value") {
				t.Fatalf("draft delivery=%#v %v", outcome, err)
			}
			if status == AgentRunStatusFailed && !strings.Contains(outcome.Content, "may be incomplete") {
				t.Fatal("partial draft presented as success")
			}
			old := "Operation “writing-session” completed successfully."
			if status == AgentRunStatusFailed {
				old = "Operation “writing-session” failed. Review the Run for the exact failed step and retry when ready."
			}
			if _, _, err := runner.postAgentResponseWithReferences(t.Context(), root, conversation, trigger, old, outcome.References, false); err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := scheduler.reconcileConversationOperationOutputs(t.Context(), scope, &ConversationRunReconcileResult{}); err != nil {
					t.Fatal(err)
				}
			}
			messages, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 3 || messages[2].Content != outcome.Content {
				t.Fatalf("recovered messages=%#v", messages)
			}
			// No second model run is needed to recover a persisted result.
			runs, _ := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, Kind: RunKindConversation})
			if len(runs) != 1 {
				t.Fatal("recovery scheduled duplicate work")
			}
		})
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
