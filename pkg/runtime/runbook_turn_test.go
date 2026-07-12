package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func runbookLiteral(value interface{}) runbook.Value {
	encoded, _ := json.Marshal(value)
	return runbook.Value{Literal: encoded}
}
func runbookRef(pointer string) *runbook.Value { value := runbook.Value{Ref: pointer}; return &value }

func TestRunbookTurnExecutesGovernedActionAndConsumesDurableResult(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "health", Version: "1", Name: "Health", Entrypoints: map[string]string{"manual": "fetch"}, Steps: map[string]runbook.Step{
		"fetch": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "web", SkillVersion: "1", Action: "fetch", Arguments: map[string]runbook.Value{"url": {Ref: "/input/url"}}, ResultPath: "/steps/fetch", Next: "done"}},
		"done":  {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"content": {Ref: "/steps/fetch/content"}}}},
	}}
	runner, err := NewRunbookTurnRunner(definition, "manual")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-1", Context: map[string]interface{}{"url": "https://example.test/health"}}
	first, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-1", Sequence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextRunStatus != AgentRunStatusRunning || len(first.ProposedActions) != 1 || first.ProposedActions[0].Capability != "web.fetch" || first.ProposedActions[0].InputRef != "/runbookActionInputs/fetch" {
		t.Fatalf("first=%#v", first)
	}
	arguments, err := resolveTurnActionInput(first.ContinuationCheckpoint, first.ProposedActions[0].InputRef)
	if err != nil || arguments["url"] != "https://example.test/health" {
		t.Fatalf("arguments=%#v err=%v", arguments, err)
	}
	first.ContinuationCheckpoint["lastAction"] = map[string]interface{}{"actionCallId": "call-1", "status": "succeeded", "result": map[string]interface{}{"content": "ok"}}
	run.Checkpoint = first.ContinuationCheckpoint
	second, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-2", Sequence: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if second.NextRunStatus != AgentRunStatusCompleted || second.RunOutput["content"] != "ok" || len(second.ProposedActions) != 0 {
		t.Fatalf("second=%#v", second)
	}
	if _, exists := second.ContinuationCheckpoint["lastAction"]; exists {
		t.Fatal("consumed action result remained in checkpoint")
	}
}

func TestRunbookTurnProposesConcurrentForkAndRunsBoundedForEach(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "deterministic", Version: "1", Name: "Deterministic", Entrypoints: map[string]string{"manual": "choose"}, Steps: map[string]runbook.Step{
		"choose": {Kind: runbook.StepDecision, Decision: &runbook.DecisionStep{Cases: []runbook.DecisionCase{{When: runbook.Predicate{Operator: runbook.PredicateTruthy, Left: runbookRef("/input/parallel")}, Next: "fork"}}, Default: "loop"}},
		"fork":   {Kind: runbook.StepFork, Fork: &runbook.ForkStep{Branches: map[string]string{"a": "a", "b": "b"}, Join: "join"}},
		"a":      {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/a": runbookLiteral(true)}, Next: "join"}},
		"b":      {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/b": runbookLiteral(true)}, Next: "join"}},
		"join":   {Kind: runbook.StepJoin, Join: &runbook.JoinStep{Fork: "fork", Mode: runbook.JoinAll, Next: "done"}},
		"loop":   {Kind: runbook.StepForEach, ForEach: &runbook.ForEachStep{Items: runbook.Value{Ref: "/input/items"}, ItemName: "item", MaxIterations: 3, Body: "copy", Next: "done"}},
		"copy":   {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/last": {Ref: "/loop/item"}}, Next: "again"}},
		"again":  {Kind: runbook.StepLoopReturn, LoopReturn: &runbook.LoopReturnStep{ForEach: "loop"}},
		"done":   {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"state": {Ref: "/state"}}}},
	}}
	parallel, _ := NewRunbookTurnRunner(definition, "manual")
	outcome, err := parallel.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "parallel", Context: map[string]interface{}{"parallel": true}}, Turn: &AgentTurn{ID: "turn", Sequence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NextRunStatus != AgentRunStatusRunning || outcome.ProposedFork == nil || outcome.ProposedFork.Policy.Mode != FanInModeAll || len(outcome.ProposedFork.Branches) != 2 {
		t.Fatalf("parallel=%#v", outcome)
	}
	for index, branch := range outcome.ProposedFork.Branches {
		child, err := parallel.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "child", Checkpoint: branch.Checkpoint}, Turn: &AgentTurn{ID: "branch", Sequence: 1}})
		if err != nil || child.NextRunStatus != AgentRunStatusCompleted || child.RunOutput["branchId"] != branch.ID {
			t.Fatalf("branch %d=%#v error=%v", index, child, err)
		}
	}
	loop, _ := NewRunbookTurnRunner(definition, "manual")
	outcome, err = loop.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "loop", Context: map[string]interface{}{"parallel": false, "items": []interface{}{"one", "two"}}}, Turn: &AgentTurn{ID: "turn", Sequence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	state := outcome.RunOutput["state"].(map[string]interface{})
	if state["last"] != "two" {
		t.Fatalf("loop=%#v", outcome)
	}
}

func TestRunbookTurnWaitsOnceAndResumesFromCheckpoint(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "wait", Version: "1", Name: "Wait", Entrypoints: map[string]string{"event": "wait"}, Steps: map[string]runbook.Step{
		"wait": {Kind: runbook.StepWait, Wait: &runbook.WaitStep{Duration: time.Minute, Next: "done"}}, "done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	runner, err := NewRunbookTurnRunner(definition, "event")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 12, 5, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	first, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run"}, Turn: &AgentTurn{ID: "one", Sequence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextRunStatus != AgentRunStatusSleeping || first.WakeCondition == nil || !first.WakeCondition.WakeAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("first=%#v", first)
	}
	second, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run", Checkpoint: first.ContinuationCheckpoint}, Turn: &AgentTurn{ID: "two", Sequence: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if second.NextRunStatus != AgentRunStatusCompleted {
		t.Fatalf("second=%#v", second)
	}
}

func TestRunbookValueRendersTypedTemplateSegments(t *testing.T) {
	value := runbook.Value{Template: []runbook.TemplateSegment{
		{Text: "Release "}, {Ref: "/input/version"}, {Text: " has metrics "}, {Ref: "/input/metrics"},
	}}
	value.Template = append(value.Template, runbook.TemplateSegment{Text: " optional="}, runbook.TemplateSegment{Ref: "/input/missing"})
	resolved, err := resolveRunbookValue(map[string]interface{}{"input": map[string]interface{}{"version": "2026.07", "metrics": map[string]interface{}{"leads": float64(12)}}}, value)
	if err != nil || resolved != `Release 2026.07 has metrics {"leads":12} optional=` {
		t.Fatalf("resolved=%#v error=%v", resolved, err)
	}
}
