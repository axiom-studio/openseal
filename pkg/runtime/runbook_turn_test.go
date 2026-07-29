package runtime

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
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
	firstTrace, err := RunbookTraceFromCheckpoint(first.ContinuationCheckpoint)
	if err != nil || len(firstTrace.Entries) != 1 || firstTrace.Entries[0].StepID != "fetch" || firstTrace.Entries[0].Status != RunbookStepTraceWaiting || firstTrace.Entries[0].Visit != 1 || firstTrace.Entries[0].TurnID != "turn-1" || !reflect.DeepEqual(firstTrace.Entries[0].InputRefs, []string{"/input/url"}) || !reflect.DeepEqual(firstTrace.Entries[0].OutputRefs, []string{"/steps/fetch"}) {
		t.Fatalf("first trace=%#v err=%v", firstTrace, err)
	}
	first.ContinuationCheckpoint["lastAction"] = map[string]interface{}{"actionCallId": "call-1", "approvalId": "approval-1", "status": "succeeded", "result": map[string]interface{}{"content": "ok"}}
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
	trace, err := RunbookTraceFromCheckpoint(second.ContinuationCheckpoint)
	if err != nil || len(trace.Entries) != 2 || trace.Entries[0].Status != RunbookStepTraceSucceeded || trace.Entries[0].ActionCallID != "call-1" || trace.Entries[0].ApprovalID != "approval-1" || trace.Entries[0].SelectedNext != "done" || trace.Entries[1].StepID != "done" || trace.Entries[1].Status != RunbookStepTraceSucceeded {
		t.Fatalf("completed trace=%#v err=%v", trace, err)
	}
	encodedTrace, _ := json.Marshal(trace)
	if strings.Contains(string(encodedTrace), "https://example.test/health") || strings.Contains(string(encodedTrace), `"content":"ok"`) {
		t.Fatalf("trace copied governed values: %s", encodedTrace)
	}
}

func TestRunbookTurnThreadsActionOutputAcrossBrowserLifecycle(t *testing.T) {
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "browser", Version: "1", Name: "Browser", Entrypoints: map[string]string{"daily": "start"},
		Steps: map[string]runbook.Step{
			"start": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "start", Arguments: map[string]runbook.Value{"profile": runbookLiteral("rowan")}, ResultPath: "/results/session", Next: "navigate",
			}},
			"navigate": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "navigate", Arguments: map[string]runbook.Value{
					"sessionId": {Ref: "/results/session/sessionId"}, "url": runbookLiteral("https://example.test"),
				}, ResultPath: "/results/navigation", Next: "close",
			}},
			"close": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "close", Arguments: map[string]runbook.Value{
					"sessionId": {Ref: "/results/session/sessionId"},
				}, ResultPath: "/results/close", Next: "done",
			}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	runner, err := NewRunbookTurnRunner(definition, "daily")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-browser", Context: map[string]interface{}{}}
	turn := func(sequence int) *TurnOutcome {
		outcome, runErr := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: fmt.Sprintf("turn-%d", sequence), Sequence: int64(sequence)}})
		if runErr != nil {
			t.Fatal(runErr)
		}
		return outcome
	}
	start := turn(1)
	start.ContinuationCheckpoint["lastAction"] = map[string]interface{}{"actionCallId": "start-call", "status": "succeeded", "result": map[string]interface{}{"sessionId": "session-1"}}
	run.Checkpoint = start.ContinuationCheckpoint

	navigate := turn(2)
	navigateArguments, err := resolveTurnActionInput(navigate.ContinuationCheckpoint, navigate.ProposedActions[0].InputRef)
	if err != nil || navigateArguments["sessionId"] != "session-1" || navigateArguments["url"] != "https://example.test" {
		t.Fatalf("navigate arguments=%#v err=%v", navigateArguments, err)
	}
	navigate.ContinuationCheckpoint["lastAction"] = map[string]interface{}{"actionCallId": "navigate-call", "status": "succeeded", "result": map[string]interface{}{"url": "https://example.test"}}
	run.Checkpoint = navigate.ContinuationCheckpoint

	closeOutcome := turn(3)
	closeArguments, err := resolveTurnActionInput(closeOutcome.ContinuationCheckpoint, closeOutcome.ProposedActions[0].InputRef)
	if err != nil || closeArguments["sessionId"] != "session-1" {
		t.Fatalf("close arguments=%#v err=%v", closeArguments, err)
	}
	closeOutcome.ContinuationCheckpoint["lastAction"] = map[string]interface{}{"actionCallId": "close-call", "status": "succeeded", "result": map[string]interface{}{"closed": true}}
	run.Checkpoint = closeOutcome.ContinuationCheckpoint
	completed := turn(4)
	if completed.NextRunStatus != AgentRunStatusCompleted || len(completed.ProposedActions) != 0 {
		t.Fatalf("completed=%#v", completed)
	}
}

func TestRunbookTurnExposesKernelRunIdentityToActionArguments(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "session", Version: "1", Name: "Session", Entrypoints: map[string]string{"start": "open"}, Steps: map[string]runbook.Step{
		"open": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "browser", SkillVersion: "1", Action: "start", Arguments: map[string]runbook.Value{
			"sessionId": {Ref: "/runtime/runId"},
		}, ResultPath: "/steps/open", Next: "done"}},
		"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	runner, err := NewRunbookTurnRunner(definition, "start")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-123", RootRunID: "root-456", ObjectiveID: "objective-789", Checkpoint: map[string]interface{}{
		"runtime": map[string]interface{}{"runId": "spoofed"},
	}}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-1", Sequence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := resolveTurnActionInput(outcome.ContinuationCheckpoint, outcome.ProposedActions[0].InputRef)
	if err != nil || arguments["sessionId"] != run.ID {
		t.Fatalf("arguments=%#v err=%v", arguments, err)
	}
	runtimeContext, _ := outcome.ContinuationCheckpoint["runtime"].(map[string]interface{})
	if runtimeContext["rootRunId"] != run.RootRunID || runtimeContext["objectiveId"] != run.ObjectiveID {
		t.Fatalf("runtime context=%#v", runtimeContext)
	}
}

func TestRunbookTurnValidatesCallableOutput(t *testing.T) {
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "typed", Version: "1", Name: "Typed",
		Entrypoints: map[string]string{"manual": "done"},
		Interfaces: map[string]runbook.Interface{"manual": {
			Description: "Return a typed result.", InputSchema: map[string]interface{}{"type": "object"},
			OutputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"result": map[string]interface{}{"type": "string"}},
				"required":   []interface{}{"result"},
			},
		}},
		Steps: map[string]runbook.Step{"done": {
			Kind: runbook.StepEnd,
			End:  &runbook.EndStep{Outputs: map[string]runbook.Value{"result": {Literal: []byte(`42`)}}},
		}},
	}
	runner, err := NewRunbookTurnRunner(definition, "manual")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Context: map[string]interface{}{}},
		Turn: &AgentTurn{ID: "turn"},
	})
	if err != nil || outcome.NextRunStatus != AgentRunStatusFailed || outcome.RunError == "" {
		t.Fatalf("output mismatch outcome=%#v err=%v", outcome, err)
	}
	trace, traceErr := RunbookTraceFromCheckpoint(outcome.ContinuationCheckpoint)
	if traceErr != nil || len(trace.Entries) != 1 || trace.Entries[0].StepID != "done" || trace.Entries[0].Status != RunbookStepTraceFailed || trace.Entries[0].Error == "" {
		t.Fatalf("failure trace=%#v err=%v", trace, traceErr)
	}
}

func TestRunbookTurnProposesConcurrentForkAndRunsBoundedForEach(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "deterministic", Version: "1", Name: "Deterministic", Entrypoints: map[string]string{"manual": "choose"}, Steps: map[string]runbook.Step{
		"choose": {Kind: runbook.StepDecision, Decision: &runbook.DecisionStep{Cases: []runbook.DecisionCase{{When: runbook.Predicate{Operator: runbook.PredicateTruthy, Left: runbookRef("/input/parallel")}, Next: "fork"}}, Default: "loop"}},
		"fork":   {Kind: runbook.StepFork, Fork: &runbook.ForkStep{Branches: map[string]string{"a": "a", "b": "b"}, BranchBudgets: map[string]runbook.BudgetAllocation{"a": {MaxTurns: 2}, "b": {MaxActions: 3}}, Join: "join"}},
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
	if outcome.ProposedFork.Branches[0].Budget == nil || outcome.ProposedFork.Branches[0].Budget.MaxTurns != 2 || outcome.ProposedFork.Branches[1].Budget == nil || outcome.ProposedFork.Branches[1].Budget.MaxActions != 3 {
		t.Fatalf("branch budgets=%#v", outcome.ProposedFork.Branches)
	}
	for index, branch := range outcome.ProposedFork.Branches {
		child, err := parallel.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "child", Checkpoint: branch.Checkpoint}, Turn: &AgentTurn{ID: "branch", Sequence: 1}})
		if err != nil || child.NextRunStatus != AgentRunStatusCompleted || child.RunOutput["branchId"] != branch.ID {
			t.Fatalf("branch %d=%#v error=%v", index, child, err)
		}
		trace, traceErr := RunbookTraceFromCheckpoint(child.ContinuationCheckpoint)
		if traceErr != nil || len(trace.Entries) != 2 || trace.Entries[0].StepID != branch.ID || trace.Entries[1].StepID != "join" {
			t.Fatalf("branch %d trace=%#v error=%v", index, trace, traceErr)
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
	firstTrace, err := RunbookTraceFromCheckpoint(first.ContinuationCheckpoint)
	if err != nil || len(firstTrace.Entries) != 1 || firstTrace.Entries[0].StepID != "wait" || firstTrace.Entries[0].Status != RunbookStepTraceWaiting {
		t.Fatalf("waiting trace=%#v err=%v", firstTrace, err)
	}
	second, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run", Checkpoint: first.ContinuationCheckpoint}, Turn: &AgentTurn{ID: "two", Sequence: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if second.NextRunStatus != AgentRunStatusCompleted {
		t.Fatalf("second=%#v", second)
	}
	trace, err := RunbookTraceFromCheckpoint(second.ContinuationCheckpoint)
	if err != nil || len(trace.Entries) != 2 || trace.Entries[0].Status != RunbookStepTraceSucceeded || trace.Entries[1].StepID != "done" || trace.Entries[1].Status != RunbookStepTraceSucceeded {
		t.Fatalf("resumed trace=%#v err=%v", trace, err)
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
