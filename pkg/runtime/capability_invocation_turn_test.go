package runtime

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// fixedActionCalls is an ActionCallReader over a fixed set of calls.
type fixedActionCalls map[string]*models.ActionCall

func (f fixedActionCalls) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	call, ok := f[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	cloned := *call
	cloned.Output = maps.Clone(call.Output)
	return &cloned, nil
}

// producerToolPath is a run-owned session path holding one terminal tool
// entry for toolCallID.
func producerToolPath(runID, toolCallID string, state models.ToolEntryState, actionCallID string) *models.AgentSessionPath {
	status := models.ToolResultSucceeded
	if state != models.ToolEntryCompleted {
		status = models.ToolResultFailed
	}
	return &models.AgentSessionPath{Entries: []*models.SessionEntry{{
		ID: "tool:step-1:" + toolCallID, RunID: runID, StepID: "step-1", Type: models.SessionEntryTool, State: state,
		ToolCallID: toolCallID, ActionCallID: actionCallID,
		Tool: &models.ToolEntry{Name: "tool", Result: &models.ToolResult{Status: status, IsError: status != models.ToolResultSucceeded}},
	}}}
}

func requireValidDeterministicOutput(t *testing.T, out agentrun.StepOutput) {
	t.Helper()
	if err := validateStepOutput(models.StepProducerDeterministic, out); err != nil {
		t.Fatalf("invalid deterministic output %#v: %v", out, err)
	}
}

func TestCapabilityInvocationProducerProposesExactActionAndCompletesWithOutput(t *testing.T) {
	action := capability.ModelAction{
		Name: "openseal.source.observe_feed", SkillID: "openseal.source", Version: "1.0.2", Action: "observe_feed",
		BindingID: "feeds", BindingRevision: 4,
	}
	calls := fixedActionCalls{}
	producer, err := NewCapabilityInvocationStepProducer([]capability.ModelAction{action}, calls)
	if err != nil {
		t.Fatal(err)
	}
	if producer.Kind() != models.StepProducerDeterministic {
		t.Fatalf("kind = %s", producer.Kind())
	}
	run := &models.AgentRun{ID: "run-1", Context: map[string]interface{}{models.CapabilityInvocationContextKey: map[string]interface{}{
		"skillId": "openseal.source", "skillVersion": "1.0.2", "action": "observe_feed",
		"inputs": map[string]interface{}{"url": "https://example.com/feed", "maxItems": float64(5)},
	}}}
	first, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step-1", Sequence: 1}, Path: &models.AgentSessionPath{}})
	if err != nil {
		t.Fatal(err)
	}
	requireValidDeterministicOutput(t, first)
	if len(first.ToolCalls) != 1 {
		t.Fatalf("first = %#v", first)
	}
	call := first.ToolCalls[0]
	if call.ID != capabilityInvocationToolCallID || call.Name != action.Name || call.Directive == nil ||
		call.Directive.IdempotencyKey != "capability-invocation:run-1" || call.Directive.BindingID != "feeds" ||
		call.Directive.BindingRevision != 4 || call.Directive.ExpectedSkillVersion != "1.0.2" || call.Directive.Summary == "" {
		t.Fatalf("tool call = %#v directive = %#v", call, call.Directive)
	}
	input, err := ParseGovernedToolInput(action, call.Arguments, &governedInputDirective{Summary: call.Directive.Summary})
	if err != nil || input.Arguments["url"] != "https://example.com/feed" || input.Arguments["maxItems"] != float64(5) || input.ReviewSummary != call.Directive.Summary {
		t.Fatalf("governed input = %#v err = %v", input, err)
	}

	calls["action-1"] = &models.ActionCall{ID: "action-1", RunID: "run-1", SkillID: "openseal.source", SkillVersion: "1.0.2", Action: "observe_feed",
		BindingID: "feeds", BindingRevision: 4, Status: models.ActionCallStatusSucceeded, Output: map[string]interface{}{"observationCount": float64(5)}}
	second, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step-2", Sequence: 2},
		Path: producerToolPath("run-1", capabilityInvocationToolCallID, models.ToolEntryCompleted, "action-1")})
	if err != nil {
		t.Fatal(err)
	}
	requireValidDeterministicOutput(t, second)
	if second.Final == nil || second.Final.Status != models.AgentRunStatusCompleted || second.Final.Answer == "" ||
		second.Final.Output["capabilityResult"].(map[string]interface{})["observationCount"] != float64(5) || second.KernelState != nil {
		t.Fatalf("second = %#v", second)
	}
}

func TestCapabilityInvocationProducerFailsRunOnUnsuccessfulCall(t *testing.T) {
	action := capability.ModelAction{Name: "source.read", SkillID: "source", Version: "1", Action: "read"}
	for _, status := range []models.ActionCallStatus{models.ActionCallStatusFailed, models.ActionCallStatusDenied, models.ActionCallStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			calls := fixedActionCalls{"action-1": {ID: "action-1", RunID: "run", SkillID: "source", SkillVersion: "1", Action: "read", Status: status}}
			producer, _ := NewCapabilityInvocationStepProducer([]capability.ModelAction{action}, calls)
			run := &models.AgentRun{ID: "run", Context: map[string]interface{}{models.CapabilityInvocationContextKey: map[string]interface{}{
				"skillId": "source", "skillVersion": "1", "action": "read", "inputs": map[string]interface{}{},
			}}}
			out, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step-2"},
				Path: producerToolPath("run", capabilityInvocationToolCallID, models.ToolEntryError, "action-1")})
			if err != nil {
				t.Fatal(err)
			}
			requireValidDeterministicOutput(t, out)
			if out.Failure == nil || out.Failure.Code != "capability_invocation_failed" || out.Failure.Explainable {
				t.Fatalf("out = %#v", out)
			}
		})
	}
	// Refused before any ActionCall row existed.
	producer, _ := NewCapabilityInvocationStepProducer([]capability.ModelAction{action}, fixedActionCalls{})
	run := &models.AgentRun{ID: "run", Context: map[string]interface{}{models.CapabilityInvocationContextKey: map[string]interface{}{
		"skillId": "source", "skillVersion": "1", "action": "read",
	}}}
	out, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step-2"},
		Path: producerToolPath("run", capabilityInvocationToolCallID, models.ToolEntryError, "")})
	if err != nil || out.Failure == nil || out.Failure.Code != "capability_invocation_refused" {
		t.Fatalf("refused out = %#v err = %v", out, err)
	}
}

func TestCapabilityInvocationProducerRejectsUnboundCapabilityAndForeignResults(t *testing.T) {
	action := capability.ModelAction{Name: "source.read", SkillID: "source", Version: "1", Action: "read"}
	calls := fixedActionCalls{
		"other-skill": {ID: "other-skill", RunID: "run", SkillID: "source", SkillVersion: "2", Action: "read", Status: models.ActionCallStatusSucceeded},
		"other-run":   {ID: "other-run", RunID: "run-2", SkillID: "source", SkillVersion: "1", Action: "read", Status: models.ActionCallStatusSucceeded},
	}
	producer, _ := NewCapabilityInvocationStepProducer([]capability.ModelAction{action}, calls)
	unbound := &models.AgentRun{ID: "run", Context: map[string]interface{}{models.CapabilityInvocationContextKey: map[string]interface{}{
		"skillId": "source", "skillVersion": "2", "action": "read", "inputs": map[string]interface{}{},
	}}}
	if _, err := producer.Produce(t.Context(), agentrun.StepInput{Run: unbound, Step: &models.AgentTurn{ID: "step"}}); err == nil {
		t.Fatal("unbound typed capability was accepted")
	}
	run := &models.AgentRun{ID: "run", Context: map[string]interface{}{models.CapabilityInvocationContextKey: map[string]interface{}{
		"skillId": "source", "skillVersion": "1", "action": "read",
	}}}
	for _, id := range []string{"other-skill", "other-run"} {
		if _, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step-2"},
			Path: producerToolPath("run", capabilityInvocationToolCallID, models.ToolEntryCompleted, id)}); err == nil {
			t.Fatalf("result %s was consumed", id)
		}
	}
	// An entry still waiting is never consumed as a result.
	if _, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step-2"},
		Path: producerToolPath("run", capabilityInvocationToolCallID, models.ToolEntryAwaitingApproval, "")}); err == nil {
		t.Fatal("open tool entry was consumed")
	}
	// Another Run's entry on the path is not this Run's call.
	out, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step-1"},
		Path: producerToolPath("run-2", capabilityInvocationToolCallID, models.ToolEntryCompleted, "other-run")})
	if err != nil || len(out.ToolCalls) != 1 {
		t.Fatalf("foreign entry changed the first step: %#v %v", out, err)
	}
	if raw := out.ToolCalls[0].Arguments; !json.Valid(raw) || string(raw) != "{}" {
		t.Fatalf("arguments = %s", raw)
	}
}
