package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TestRunbookProducerReportsDelegatedAgentFailureReason(t *testing.T) {
	producer := &RunbookStepProducer{definition: &runbook.Definition{}}
	run := &AgentRun{ID: "run-1", Scope: Scope{Kind: "tenant", ID: "1"}, Output: map[string]interface{}{}}
	stepID := "perform-hourly"
	requestID := StableCollaborationID(run.Scope, delegationIdempotencyKey(run.ID, stepID), "request")
	run.Output["collaborationResults"] = map[string]interface{}{
		requestID: map[string]interface{}{
			"status": string(AgentRequestStatusFailed),
			"reason": "Model provider quota is exhausted; choose a credential with available quota.",
		},
	}
	state := &runbookExecutionState{PendingDelegation: stepID}
	consumed, failure, err := producer.consumeDelegationResult(map[string]interface{}{}, state, run)
	if err != nil || consumed || !strings.Contains(failure, "provider quota is exhausted") {
		t.Fatalf("consumed=%t failure=%q err=%v", consumed, failure, err)
	}
}

func TestRunbookProducerEmitsPinnedGovernedCallAndConsumesActionCall(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "health", Version: "1", Name: "Health", Entrypoints: map[string]string{"manual": "fetch"}, Steps: map[string]runbook.Step{
		"fetch": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "web", SkillVersion: "1", Action: "fetch", Arguments: map[string]runbook.Value{"url": {Ref: "/input/url"}}, ResultPath: "/steps/fetch", Next: "done"}},
		"done":  {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"content": {Ref: "/steps/fetch/content"}}}},
	}}
	calls := modelsteptest.FixedActionCalls{}
	producer := newTestRunbookProducer(t, definition, "manual", calls)
	if producer.Kind() != StepProducerDeterministic {
		t.Fatalf("kind = %s", producer.Kind())
	}
	action := webFetchAction()
	binding := &agentrun.TurnRunnerBinding{ModelActions: []capability.ModelAction{action}}
	run := &AgentRun{ID: "run-1", Context: map[string]interface{}{"url": "https://example.test/health"}}
	path := &AgentSessionPath{}
	first := produceRunbook(t, producer, runbookInput(run, 1, binding, path))
	if len(first.ToolCalls) != 1 {
		t.Fatalf("first = %#v", first)
	}
	call := first.ToolCalls[0]
	if call.ID != "runbook.1.fetch" || call.Name != action.Name || call.Directive == nil ||
		call.Directive.IdempotencyKey != "runbook:health:run-1:fetch:1" || call.Directive.BindingID != "web-binding" ||
		call.Directive.BindingRevision != 3 || call.Directive.ExpectedSkillVersion != "1" || call.Directive.Summary != "Execute web.fetch" {
		t.Fatalf("call = %#v directive = %#v", call, call.Directive)
	}
	var arguments map[string]interface{}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil || arguments["url"] != "https://example.test/health" {
		t.Fatalf("arguments = %s err = %v", call.Arguments, err)
	}
	if _, legacy := first.KernelState["runbookActionInputs"]; legacy {
		t.Fatal("action inputs were copied into the engine state")
	}
	firstTrace, err := RunbookTraceFromCheckpoint(first.KernelState)
	if err != nil || len(firstTrace.Entries) != 1 || firstTrace.Entries[0].StepID != "fetch" || firstTrace.Entries[0].Status != RunbookStepTraceWaiting || firstTrace.Entries[0].Visit != 1 || firstTrace.Entries[0].TurnID != "step-1" || !reflect.DeepEqual(firstTrace.Entries[0].InputRefs, []string{"/input/url"}) || !reflect.DeepEqual(firstTrace.Entries[0].OutputRefs, []string{"/steps/fetch"}) {
		t.Fatalf("first trace=%#v err=%v", firstTrace, err)
	}
	if len(firstTrace.Entries[0].Inputs) != 1 || firstTrace.Entries[0].Inputs[0].Shape != "string" || len(firstTrace.Entries[0].Outputs) != 1 || firstTrace.Entries[0].Outputs[0].Availability != RunbookDataMissing {
		t.Fatalf("first data observations=%#v", firstTrace.Entries[0])
	}
	// The loop applies the step, the inline executor records the call.
	run.Checkpoint = first.KernelState
	calls["call-1"] = &ActionCall{ID: "call-1", RunID: run.ID, SkillID: "web", SkillVersion: "1", Action: "fetch", ApprovalID: "approval-1", Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"content": "ok"}}
	appendRunbookToolResult(path, run.ID, call.ID, ToolEntryCompleted, "call-1")
	second := produceRunbook(t, producer, runbookInput(run, 2, binding, path))
	if second.Final == nil || second.Final.Status != AgentRunStatusCompleted || second.Final.Output["content"] != "ok" || len(second.ToolCalls) != 0 {
		t.Fatalf("second=%#v", second)
	}
	state, err := decodeRunbookState(second.KernelState)
	if err != nil || state.PendingAction != "" || state.PendingCall != "" {
		t.Fatalf("pending call survived: %#v err=%v", state, err)
	}
	trace, err := RunbookTraceFromCheckpoint(second.KernelState)
	if err != nil || len(trace.Entries) != 2 || trace.Entries[0].Status != RunbookStepTraceSucceeded || trace.Entries[0].ActionCallID != "call-1" || trace.Entries[0].ApprovalID != "approval-1" || trace.Entries[0].SelectedNext != "done" || trace.Entries[1].StepID != "done" || trace.Entries[1].Status != RunbookStepTraceSucceeded {
		t.Fatalf("completed trace=%#v err=%v", trace, err)
	}
	if trace.Entries[0].Outputs[0].Shape != "object" || trace.Entries[1].Inputs[0].Shape != "string" || trace.Entries[1].Outputs[0].Shape != "string" {
		t.Fatalf("completed data observations=%#v", trace.Entries)
	}
	encodedTrace, _ := json.Marshal(trace)
	if strings.Contains(string(encodedTrace), "https://example.test/health") || strings.Contains(string(encodedTrace), `"content":"ok"`) {
		t.Fatalf("trace copied governed values: %s", encodedTrace)
	}
}

func TestRunbookProducerEmitsKernelForkCallAndRunsBoundedForEach(t *testing.T) {
	definition := concurrentRunbookDefinition()
	parallel := newTestRunbookProducer(t, definition, "manual", nil)
	run := &AgentRun{ID: "parallel", Context: map[string]interface{}{"parallel": true}}
	out := produceRunbook(t, parallel, runbookInput(run, 1, nil, nil))
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != string(ControlToolRunbookFork) || out.ToolCalls[0].Directive == nil {
		t.Fatalf("parallel=%#v", out)
	}
	if string(out.ToolCalls[0].Arguments) != `{"step":"fork","visit":2}` || out.ToolCalls[0].ID != "runbook.2.fork" {
		t.Fatalf("fork call=%#v args=%s", out.ToolCalls[0], out.ToolCalls[0].Arguments)
	}
	state, err := decodeRunbookState(out.KernelState)
	if err != nil || state.Current != "fork" || state.PendingFork != "fork" || state.PendingCall != out.ToolCalls[0].ID {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	// The settle rebuilds the spec from the applied engine state.
	spec, err := parallel.forkSpec(out.KernelState, state, run.ID)
	if err != nil || spec.ForkID != "fork" || spec.Policy.Mode != FanInModeAll || len(spec.Branches) != 2 || spec.SourceCheckpoint == nil || spec.ChildKind != "" {
		t.Fatalf("spec=%#v err=%v", spec, err)
	}
	if spec.Branches[0].Budget == nil || spec.Branches[0].Budget.MaxTurns != 2 || spec.Branches[1].Budget == nil || spec.Branches[1].Budget.MaxActions != 3 {
		t.Fatalf("branch budgets=%#v", spec.Branches)
	}
	for index, branch := range spec.Branches {
		if _, copied := branch.Checkpoint[runbookTraceCheckpointKey]; copied {
			t.Fatalf("branch %d copied the parent trace", index)
		}
		child := produceRunbook(t, parallel, runbookInput(&AgentRun{ID: "child", Checkpoint: branch.Checkpoint}, 1, nil, nil))
		if child.Final == nil || child.Final.Output["branchId"] != branch.ID || child.Final.Output["forkId"] != "fork" {
			t.Fatalf("branch %d=%#v", index, child)
		}
		trace, traceErr := RunbookTraceFromCheckpoint(child.KernelState)
		if traceErr != nil || len(trace.Entries) != 2 || trace.Entries[0].StepID != branch.ID || trace.Entries[1].StepID != "join" {
			t.Fatalf("branch %d trace=%#v error=%v", index, trace, traceErr)
		}
	}
	// The group resolution writes the joined branches into Run.Output.
	groupID := agentrun.StableForkIdentifier(run.ID, "fork", "group")
	run.Checkpoint = out.KernelState
	run.Output = map[string]interface{}{"dependencyGroups": map[string]interface{}{groupID: map[string]interface{}{
		"status": string(RunDependencyGroupSatisfied),
		"dependencies": map[string]interface{}{
			"a": map[string]interface{}{"state": string(RunDependencyStateSatisfied), "result": map[string]interface{}{"checkpoint": map[string]interface{}{"state": map[string]interface{}{"a": true}}}},
			"b": map[string]interface{}{"state": string(RunDependencyStateSatisfied), "result": map[string]interface{}{"checkpoint": map[string]interface{}{"state": map[string]interface{}{"b": true}}}},
		},
	}}}
	joined := produceRunbook(t, parallel, runbookInput(run, 2, nil, nil))
	if joined.Final == nil || !reflect.DeepEqual(joined.Final.Output["state"], map[string]interface{}{"a": true, "b": true}) {
		t.Fatalf("joined=%#v", joined)
	}
	loop := newTestRunbookProducer(t, definition, "manual", nil)
	looped := produceRunbook(t, loop, runbookInput(&AgentRun{ID: "loop", Context: map[string]interface{}{"parallel": false, "items": []interface{}{"one", "two"}}}, 1, nil, nil))
	if looped.Final == nil || looped.Final.Output["state"].(map[string]interface{})["last"] != "two" {
		t.Fatalf("loop=%#v", looped)
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

func TestRunbookKernelCallRefusesStateItWasNotPlannedFor(t *testing.T) {
	store := newTestKernelStore(t)
	control := &ControlExecutor{Portfolio: store, Sessions: store, Forks: agentrun.NewRunForkCoordinator(store), Collaboration: NewCollaborationService(store)}
	producer := newTestRunbookProducer(t, concurrentRunbookDefinition(), "manual", store)
	cc := agentrun.ControlContext{
		Run:     &AgentRun{ID: "run", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "fork", "pendingFork": "fork", "pendingCall": "runbook.2.fork"}}},
		Binding: &agentrun.TurnRunnerBinding{Producer: producer}, Origin: ControlOriginKernel,
		Entry:     &SessionEntry{ToolCallID: "runbook.9.fork"},
		Arguments: &runbookKernelArgs{Step: "fork", Visit: 9},
	}
	pending := func(s runbookExecutionState) string { return s.PendingFork }
	if _, _, _, err := runbookKernelCall(cc, pending); err == nil {
		t.Fatal("a stale kernel call settled against newer engine state")
	}
	cc.Entry.ToolCallID = "runbook.2.fork"
	if _, _, _, err := runbookKernelCall(cc, pending); err != nil {
		t.Fatalf("planned call refused: %v", err)
	}
	cc.Origin = ControlOriginModel
	if _, _, _, err := runbookKernelCall(cc, pending); err == nil {
		t.Fatal("a model call reached the runbook engine")
	}
	cc.Origin, cc.Entry = ControlOriginKernel, nil
	if _, _, _, err := runbookKernelCall(cc, pending); err == nil {
		t.Fatal("a kernel call without its tool entry settled")
	}
	result, err := (runbookForkHandler{}).Run(t.Context(), control, cc)
	if err == nil || result.Entry != nil {
		t.Fatalf("an entry-less fork call was not refused: %#v", result)
	}
}

func TestRunbookProducerKeepsModelMediaAndOversizeOutputOutOfCheckpoint(t *testing.T) {
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "shots", Version: "1", Name: "Shots", Entrypoints: map[string]string{"daily": "shot"},
		Steps: map[string]runbook.Step{
			"shot": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "screenshot", ResultPath: "/results/shot", Next: "dump",
			}},
			"dump": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "dump", ResultPath: "/results/dump", Next: "done",
			}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	calls := modelsteptest.FixedActionCalls{}
	producer := newTestRunbookProducer(t, definition, "daily", calls)
	binding := &agentrun.TurnRunnerBinding{ModelActions: []capability.ModelAction{
		{Name: "browser_screenshot", SkillID: "browser", Version: "1", Action: "screenshot"},
		{Name: "browser_dump", SkillID: "browser", Version: "1", Action: "dump"},
	}}
	run := &AgentRun{ID: "run-shots", Context: map[string]interface{}{}}
	path := &AgentSessionPath{}
	step := func(sequence int64) agentrun.StepOutput {
		out := produceRunbook(t, producer, runbookInput(run, sequence, binding, path))
		run.Checkpoint = out.KernelState
		return out
	}
	image := strings.Repeat("iVBORw0KGgo", 20000)
	screenshot := map[string]interface{}{
		"url": "https://example.test",
		"modelMedia": map[string]interface{}{
			"images": []interface{}{map[string]interface{}{"mediaType": "image/png", "contentBase64": image}},
		},
	}
	shot := step(1)
	calls["shot-call"] = &ActionCall{ID: "shot-call", RunID: run.ID, Status: ActionCallStatusSucceeded, Output: screenshot}
	appendRunbookToolResult(path, run.ID, shot.ToolCalls[0].ID, ToolEntryCompleted, "shot-call")
	dump := step(2)
	if len(dump.ToolCalls) != 1 || dump.ToolCalls[0].Name != "browser_dump" {
		t.Fatalf("dump=%#v", dump)
	}
	if _, ok := screenshot["modelMedia"]; !ok {
		t.Fatal("the stored ActionCall output was mutated")
	}
	rows := make([]interface{}, 5000)
	for index := range rows {
		rows[index] = map[string]interface{}{"index": index, "text": strings.Repeat("x", 100)}
	}
	calls["dump-call"] = &ActionCall{ID: "dump-call", RunID: run.ID, Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"rows": rows}}
	appendRunbookToolResult(path, run.ID, dump.ToolCalls[0].ID, ToolEntryCompleted, "dump-call")
	if done := step(3); done.Final == nil || done.Final.Status != AgentRunStatusCompleted {
		t.Fatalf("done=%#v", done)
	}
	encoded, err := json.Marshal(run.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "contentBase64") || strings.Contains(string(encoded), "modelMedia") {
		t.Fatal("model media reached the runbook checkpoint")
	}
	if len(encoded) > 2*runbookActionResultLimit {
		t.Fatalf("checkpoint is %d bytes", len(encoded))
	}
	results, _ := run.Checkpoint["results"].(map[string]interface{})
	shotResult, _ := results["shot"].(map[string]interface{})
	dumpResult, _ := results["dump"].(map[string]interface{})
	if shotResult["url"] != "https://example.test" || dumpResult["truncated"] != true || dumpResult["available"] != true {
		t.Fatalf("results=%#v", results)
	}
}
