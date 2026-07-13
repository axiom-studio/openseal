package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestCapabilityInvocationTurnRunnerProposesExactAuthorizedActionAndConsumesResult(t *testing.T) {
	runner, err := NewCapabilityInvocationTurnRunner([]capability.ModelAction{{
		Name: "openseal.source.observe_feed", SkillID: "openseal.source", Version: "1.0.2", Action: "observe_feed",
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-1", Context: map[string]interface{}{capabilityInvocationContextKey: map[string]interface{}{
		"skillId": "openseal.source", "skillVersion": "1.0.2", "action": "observe_feed",
		"inputs": map[string]interface{}{"url": "https://example.com/feed", "maxItems": float64(5)},
	}}}
	first, err := runner.RunTurn(context.Background(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-1", Sequence: 1}})
	if err != nil || first.NextRunStatus != AgentRunStatusRunning || len(first.ProposedActions) != 1 ||
		first.ProposedActions[0].Capability != "openseal.source.observe_feed" || first.ProposedActions[0].InputRef != "/capabilityActionInputs/requested" ||
		first.ContinuationCheckpoint["capabilityActionInputs"].(map[string]interface{})["requested"].(map[string]interface{})["url"] != "https://example.com/feed" {
		t.Fatalf("first outcome = %#v, err = %v", first, err)
	}
	run.Checkpoint = first.ContinuationCheckpoint
	run.Checkpoint["lastAction"] = map[string]interface{}{
		"skillId": "openseal.source", "skillVersion": "1.0.2", "action": "observe_feed", "status": "succeeded",
		"result": map[string]interface{}{"observationCount": float64(5)},
	}
	second, err := runner.RunTurn(context.Background(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-2", Sequence: 2}})
	if err != nil || second.NextRunStatus != AgentRunStatusCompleted || len(second.ProposedActions) != 0 ||
		second.RunOutput["capabilityResult"].(map[string]interface{})["observationCount"] != float64(5) ||
		second.ContinuationCheckpoint["lastAction"] != nil || second.ContinuationCheckpoint["capabilityActionInputs"] != nil {
		t.Fatalf("second outcome = %#v, err = %v", second, err)
	}
}

func TestCapabilityInvocationTurnRunnerRejectsUnboundCapability(t *testing.T) {
	runner, _ := NewCapabilityInvocationTurnRunner([]capability.ModelAction{{Name: "source.read", SkillID: "source", Version: "1", Action: "read"}})
	_, err := runner.RunTurn(context.Background(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Context: map[string]interface{}{capabilityInvocationContextKey: map[string]interface{}{
			"skillId": "source", "skillVersion": "2", "action": "read", "inputs": map[string]interface{}{},
		}}}, Turn: &AgentTurn{ID: "turn"},
	})
	if err == nil {
		t.Fatal("unbound typed capability was accepted")
	}
}
