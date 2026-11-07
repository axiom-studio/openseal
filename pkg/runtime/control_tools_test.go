package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// fakeControlArgs is the strict argument shape of the fake handlers.
type fakeControlArgs struct {
	Goal string `json:"goal"`
}

type fakeControlHandler struct {
	calls int
	seen  []interface{}
}

func (h *fakeControlHandler) spec(a ControlAuthority) ControlToolSpec {
	return ControlToolSpec{Description: "fake", InputSchema: map[string]interface{}{"type": "object"}}
}

func (h *fakeControlHandler) decode(raw json.RawMessage) (interface{}, error) {
	return decodeControlInto[fakeControlArgs](raw)
}

func (h *fakeControlHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	h.calls++
	h.seen = append(h.seen, cc.Arguments)
	return x.complete(ctx, cc, controlJSONResult(ToolResultSucceeded, map[string]interface{}{"status": "done"}), &ControlEntryMeta{ForkID: "fork-" + cc.Entry.ID})
}

func fakeControlRegistry() (controlRegistry, map[ControlToolName]*fakeControlHandler) {
	registry := controlRegistry{}
	handlers := map[ControlToolName]*fakeControlHandler{}
	for _, name := range controlToolOrder {
		h := &fakeControlHandler{}
		handlers[name] = h
		registry.register(name, h)
	}
	return registry, handlers
}

func specNames(specs []ControlToolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, string(spec.Name))
	}
	return names
}

func TestControlToolSpecsStableOrderAndPresence(t *testing.T) {
	registry, _ := fakeControlRegistry()
	everything := ControlAuthority{
		CanReportProgress: true, CanWait: true, CanAskUser: true, CanReact: true,
		TaskStart:       &HostedConversationTaskContext{ConversationID: "c1", CanStart: true},
		ForkAllowed:     true,
		Operations:      []HostedRunbookOperation{{Name: "op"}},
		DelegateTargets: []HostedAgentTarget{{ID: "a2", DisplayName: "Other"}},
	}
	cases := []struct {
		name string
		edit func(*ControlAuthority)
		want []string
	}{
		{"everything", func(*ControlAuthority) {}, []string{
			"turn_final_answer", "turn_report_progress", "wait", "ask_user", "react", "conversation_start_task",
			"control_fork", "run_operation", "agent_delegate", "agent_delegate_reply",
		}},
		{"zero authority offers only the final answer", func(a *ControlAuthority) { *a = ControlAuthority{} }, []string{"turn_final_answer"}},
		{"spoken and evidence keep the default final tool", func(a *ControlAuthority) {
			*a = ControlAuthority{FinalMode: FinalModeSpoken, CanReact: true}
		}, []string{"turn_final_answer", "react"}},
		{"voice call hides task start", func(a *ControlAuthority) { a.VoiceCallStarted = true }, []string{
			"turn_final_answer", "turn_report_progress", "wait", "ask_user", "react",
			"control_fork", "run_operation", "agent_delegate", "agent_delegate_reply",
		}},
		{"task context that cannot start", func(a *ControlAuthority) {
			a.TaskStart = &HostedConversationTaskContext{ConversationID: "c1"}
			a.Operations = nil
			a.DelegateTargets = nil
		}, []string{"turn_final_answer", "turn_report_progress", "wait", "ask_user", "react", "control_fork"}},
		{"decision Run offers only its tool", func(a *ControlAuthority) { a.FinalMode = FinalModeDecision }, []string{"agent_request_decision"}},
		{"review Run offers only its tool", func(a *ControlAuthority) { a.FinalMode = FinalModeReview }, []string{"agent_request_review"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authority := everything
			tc.edit(&authority)
			specs := registry.specs(authority)
			if got := specNames(specs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("specs = %v, want %v", got, tc.want)
			}
			// The list is a pure function of the authority (prompt cache).
			if again := specNames(registry.specs(authority)); !reflect.DeepEqual(again, tc.want) {
				t.Fatalf("second listing = %v", again)
			}
			for _, spec := range specs {
				if spec.Phase != controlToolRules[spec.Name].Phase {
					t.Fatalf("%s phase = %s", spec.Name, spec.Phase)
				}
				tool := spec.ModelStepTool()
				if tool.Group != ModelStepToolHarness || tool.Name != string(spec.Name) {
					t.Fatalf("model step tool = %#v", tool)
				}
			}
		})
	}
	// Unregistered tools are never offered.
	partial := controlRegistry{}
	partial.register(ControlToolWait, &fakeControlHandler{})
	if got := specNames(partial.specs(everything)); !reflect.DeepEqual(got, []string{"wait"}) {
		t.Fatalf("partial registry specs = %v", got)
	}
	// Every ordered name has a rule, and every rule is ordered once.
	if len(controlToolOrder) != len(controlToolRules) {
		t.Fatalf("order has %d names, rules %d", len(controlToolOrder), len(controlToolRules))
	}
	for _, name := range controlToolOrder {
		if !IsControlTool(string(name)) {
			t.Fatalf("%s has no rule", name)
		}
	}
	if IsControlTool("search_capabilities") || IsControlTool("linear_get_issue") {
		t.Fatal("non-control tools reported as control tools")
	}
}

func modelCalls(names ...string) []ModelToolCall {
	calls := make([]ModelToolCall, 0, len(names))
	for i, name := range names {
		calls = append(calls, ModelToolCall{ID: "call-" + string(rune('a'+i)), Name: name, Arguments: json.RawMessage(`{}`)})
	}
	return calls
}

func requireControlError(t *testing.T, result *ToolResult, status ToolResultStatus, code string) map[string]interface{} {
	t.Helper()
	if result == nil || !result.IsError || result.Status != status || len(result.Content) != 1 || result.Content[0].Type != ContentPartJSON {
		t.Fatalf("result = %#v, want %s is_error json", result, status)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(result.Content[0].Value, &value); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if value["error"] != code || value["status"] != string(status) || strings.TrimSpace(value["message"].(string)) == "" {
		t.Fatalf("result value = %v, want error %s", value, code)
	}
	return value
}

func TestControlSecondSettleCallInSameMessageIsRefused(t *testing.T) {
	calls := modelCalls("control_fork", "react", "ask_user", "linear_get_issue", "wait")
	refusals := controlCallRefusals(calls)
	if len(refusals) != 2 || refusals["call-a"] != nil || refusals["call-b"] != nil || refusals["call-d"] != nil {
		t.Fatalf("refusals = %v", refusals)
	}
	value := requireControlError(t, refusals["call-c"], ToolResultRefused, "control_one_settle_call")
	if value["message"] != "Call ask_user alone in its own message after this one returns." {
		t.Fatalf("message = %v", value["message"])
	}
	requireControlError(t, refusals["call-e"], ToolResultRefused, "control_one_settle_call")
	if got := controlSettleConflicts(calls); !reflect.DeepEqual(got, []int{2, 4}) {
		t.Fatalf("conflicts = %v", got)
	}
	// One settle call among inline and governed calls is fine.
	if refusals := controlCallRefusals(modelCalls("react", "control_fork", "linear_get_issue")); len(refusals) != 0 {
		t.Fatalf("single settle call refused: %v", refusals)
	}
}

func TestFinalAnswerWithOtherCallsIsRefusedOthersRun(t *testing.T) {
	refusals := controlCallRefusals(modelCalls("linear_get_issue", "turn_final_answer", "react"))
	if len(refusals) != 1 {
		t.Fatalf("refusals = %v", refusals)
	}
	value := requireControlError(t, refusals["call-b"], ToolResultRefused, "final_answer_not_alone")
	if value["message"] != "Call turn_final_answer alone after your other tools have returned." {
		t.Fatalf("message = %v", value["message"])
	}
	// A refused final does not take the settle slot: the fork still settles.
	refusals = controlCallRefusals(modelCalls("turn_final_answer", "control_fork"))
	if len(refusals) != 1 || refusals["call-b"] != nil {
		t.Fatalf("refusals = %v", refusals)
	}
	requireControlError(t, refusals["call-a"], ToolResultRefused, "final_answer_not_alone")
	// Task start must also be alone (A4).
	refusals = controlCallRefusals(modelCalls("conversation_start_task", "react"))
	if len(refusals) != 1 {
		t.Fatalf("refusals = %v", refusals)
	}
	requireControlError(t, refusals["call-a"], ToolResultRefused, "control_not_alone")
	// Alone, the final answer settles.
	if refusals := controlCallRefusals(modelCalls("turn_final_answer")); len(refusals) != 0 {
		t.Fatalf("lone final refused: %v", refusals)
	}
}

// controlFixture is a claimed Run with a session holding one finished step
// whose assistant entry made the given calls (all entries pending).
type controlFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *MemoryStore
	run      *AgentRun
	session  *AgentSession
	step     *AgentTurn
	entries  map[string]*SessionEntry
	exec     *ControlExecutor
	handlers map[ControlToolName]*fakeControlHandler
	now      time.Time
}

const controlTestWorker = "control-worker"

func newControlFixture(t *testing.T, calls []LoopToolCall) *controlFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "control-" + uuid.NewString()[:8]}
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	created, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}, AssignedAgentID: "agent-1",
		Goal: "work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, WorkerID: controlTestWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	opened, err := store.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
		Scope: scope, OwnerRunID: run.ID, AgentID: "agent-1",
		Lease: AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: now}, Now: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	stepID := "step-1"
	assistant := &SessionEntry{
		ID: SessionEntryID(opened.Session.ID, AssistantEntryKey(stepID)), RunID: run.ID, StepID: stepID, Step: 1,
		Type: SessionEntryAssistant, Assistant: &AssistantEntry{
			Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopToolCalls,
		},
	}
	batch := []*SessionEntry{assistant}
	for _, call := range calls {
		entryStep := stepID
		if call.Directive == nil {
			assistant.Assistant.ToolCalls = append(assistant.Assistant.ToolCalls, ModelToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		} else {
			// Deterministic calls are appended without an assistant entry,
			// under their own step.
			entryStep = "step-kernel"
		}
		entry := &SessionEntry{
			ID: SessionEntryID(opened.Session.ID, ToolEntryKey(entryStep, call.ID)), RunID: run.ID, StepID: entryStep, Step: 1,
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: call.ID,
			Tool: &ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: ToolSideEffectWrite},
		}
		if call.Directive != nil {
			entry.Tool.Directive, _ = json.Marshal(call.Directive)
		}
		batch = append(batch, entry)
	}
	appended, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: scope, SessionID: opened.Session.ID, RunID: run.ID,
		Lease: &AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: now}, Entries: batch, Now: now,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	run, err = store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	run.LastAppliedTurn = 1
	f := &controlFixture{
		t: t, ctx: ctx, store: store, run: run, session: opened.Session, now: now,
		step:    &AgentTurn{ID: stepID, RunID: run.ID, Sequence: 1, Status: AgentTurnStatusCompleted},
		entries: map[string]*SessionEntry{},
	}
	for _, entry := range appended.Entries {
		if entry.Type == SessionEntryTool {
			f.entries[entry.ToolCallID] = entry
		}
	}
	registry, handlers := fakeControlRegistry()
	f.handlers = handlers
	f.exec = &ControlExecutor{Store: store, Sessions: store, Now: func() time.Time { return now }, handlers: registry}
	return f
}

func (f *controlFixture) context(callID string, authority ControlAuthority) ControlContext {
	entry := f.entries[callID]
	if entry == nil {
		f.t.Fatalf("no entry for call %s", callID)
	}
	return NewControlContext(StepToolsInput{
		Run: f.run, Step: f.step, Session: f.session, WorkerID: controlTestWorker,
		Binding: &TurnRunnerBinding{Authority: authority},
	}, entry)
}

func (f *controlFixture) stored(callID string) *SessionEntry {
	f.t.Helper()
	entry, err := f.store.GetSessionEntry(f.ctx, f.run.Scope, f.entries[callID].ID)
	if err != nil {
		f.t.Fatalf("get entry: %v", err)
	}
	return entry
}

func TestControlArgumentsRejectUnknownFields(t *testing.T) {
	f := newControlFixture(t, []LoopToolCall{
		{ID: "unknown", Name: "control_fork", Arguments: json.RawMessage(`{"goal":"x","budget":3}`)},
		{ID: "string", Name: "wait", Arguments: json.RawMessage(`{"goal":3}`)},
		{ID: "ok", Name: "ask_user", Arguments: json.RawMessage(`{"goal":"x"}`)},
	})
	authority := ControlAuthority{ForkAllowed: true, CanWait: true, CanAskUser: true}
	for _, callID := range []string{"unknown", "string"} {
		result, err := f.exec.Settle(f.ctx, f.context(callID, authority))
		if err != nil {
			t.Fatalf("settle %s: %v", callID, err)
		}
		if result.Kind != ControlContinue || result.Entry.State != ToolEntryError {
			t.Fatalf("%s result = %#v", callID, result)
		}
		requireControlError(t, f.stored(callID).Tool.Result, ToolResultFailed, "tool_arguments_invalid")
	}
	if f.handlers[ControlToolFork].calls != 0 || f.handlers[ControlToolWait].calls != 0 {
		t.Fatal("a handler ran with invalid arguments")
	}
	result, err := f.exec.Settle(f.ctx, f.context("ok", authority))
	if err != nil {
		t.Fatalf("settle ok: %v", err)
	}
	if result.Kind != ControlContinue || result.Entry.State != ToolEntryCompleted {
		t.Fatalf("ok result = %#v", result)
	}
	if args, _ := f.handlers[ControlToolAskUser].seen[0].(*fakeControlArgs); args == nil || args.Goal != "x" {
		t.Fatalf("decoded arguments = %#v", f.handlers[ControlToolAskUser].seen)
	}
	// One value only: a trailing value is refused (stored entries always hold
	// one object, so this is checked on the decoder).
	if _, err := decodeControlInto[fakeControlArgs](json.RawMessage(`{"goal":"x"} {"goal":"y"}`)); err == nil {
		t.Fatal("trailing value accepted")
	}
	if args, err := decodeControlInto[fakeControlArgs](nil); err != nil || args.(*fakeControlArgs).Goal != "" {
		t.Fatalf("empty arguments = %#v, %v", args, err)
	}
	meta, err := DecodeControlEntryMeta(f.stored("ok"))
	if err != nil || meta == nil || meta.Kind != ControlToolAskUser || meta.ForkID != "fork-"+f.entries["ok"].ID {
		t.Fatalf("meta = %#v, %v", meta, err)
	}
}

func TestControlToolNotOfferedIsError(t *testing.T) {
	f := newControlFixture(t, []LoopToolCall{
		{ID: "fork", Name: "control_fork", Arguments: json.RawMessage(`{"goal":"x"}`)},
		{ID: "decide", Name: "agent_request_decision", Arguments: json.RawMessage(`{}`)},
		{ID: "kernel", Name: "agent_delegate", Arguments: json.RawMessage(`{"goal":"x"}`),
			Directive: &ToolCallDirective{IdempotencyKey: "runbook-step"}},
	})
	authority := ControlAuthority{CanWait: true}
	for _, callID := range []string{"fork", "decide"} {
		result, err := f.exec.Settle(f.ctx, f.context(callID, authority))
		if err != nil {
			t.Fatalf("settle %s: %v", callID, err)
		}
		if result.Kind != ControlContinue || result.Entry.State != ToolEntryError {
			t.Fatalf("%s result = %#v", callID, result)
		}
		value := requireControlError(t, f.stored(callID).Tool.Result, ToolResultRefused, "tool_not_available")
		if value["tool"] != f.entries[callID].Tool.Name {
			t.Fatalf("tool = %v", value["tool"])
		}
	}
	if f.handlers[ControlToolFork].calls != 0 || f.handlers[ControlToolRequestDecision].calls != 0 {
		t.Fatal("a handler ran for a tool that was not offered")
	}
	// A kernel-origin call (runbook directive) is preauthorized.
	cc := f.context("kernel", authority)
	if cc.Origin != ControlOriginKernel {
		t.Fatalf("origin = %s", cc.Origin)
	}
	result, err := f.exec.Settle(f.ctx, cc)
	if err != nil || result.Entry.State != ToolEntryCompleted || f.handlers[ControlToolDelegate].calls != 1 {
		t.Fatalf("kernel call = %#v, %v", result, err)
	}
}

func TestControlDispatcherPhaseAndStateRules(t *testing.T) {
	f := newControlFixture(t, []LoopToolCall{
		{ID: "react", Name: "react", Arguments: json.RawMessage(`{}`)},
		{ID: "wait", Name: "wait", Arguments: json.RawMessage(`{}`)},
		{ID: "gov", Name: "linear_get_issue", Arguments: json.RawMessage(`{}`)},
	})
	authority := ControlAuthority{CanReact: true, CanWait: true}
	// Phase mismatch is a loop bug.
	if _, err := f.exec.Settle(f.ctx, f.context("react", authority)); err == nil {
		t.Fatal("settle of an inline tool succeeded")
	}
	if _, err := f.exec.Inline(f.ctx, f.context("wait", authority)); err == nil {
		t.Fatal("inline run of a settle tool succeeded")
	}
	if _, err := f.exec.Settle(f.ctx, f.context("gov", authority)); err == nil {
		t.Fatal("dispatch of a governed tool succeeded")
	}
	// Settle needs the finished and applied step.
	cc := f.context("wait", authority)
	cc.Step = &AgentTurn{ID: "step-1", Sequence: 1, Status: AgentTurnStatusRunning}
	if _, err := f.exec.Settle(f.ctx, cc); !errors.Is(err, ErrControlEntryState) {
		t.Fatalf("settle on running step = %v", err)
	}
	// A lost lease aborts the claim.
	cc = f.context("wait", authority)
	cc.WorkerID = "other"
	if _, err := f.exec.Settle(f.ctx, cc); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("settle under another worker = %v", err)
	}
	// Inline runs at any time in the step.
	result, err := f.exec.Inline(f.ctx, f.context("react", authority))
	if err != nil || result.Entry.State != ToolEntryCompleted {
		t.Fatalf("inline react = %#v, %v", result, err)
	}
	// A terminal entry is not run again; resume needs a parked entry.
	stale := f.context("react", authority)
	stale.Entry = result.Entry
	if _, err := f.exec.Inline(f.ctx, stale); !errors.Is(err, ErrControlEntryState) {
		t.Fatalf("inline on terminal entry = %v", err)
	}
	if _, err := f.exec.Resume(f.ctx, f.context("wait", authority)); !errors.Is(err, ErrControlEntryState) {
		t.Fatalf("resume on pending entry = %v", err)
	}
}

func TestControlSettleEntryPicksOpenSettleCall(t *testing.T) {
	entries := []*SessionEntry{
		testToolEntry("t1", "s1", "c1", ToolEntryPending),
		{ID: "t2", Type: SessionEntryTool, State: ToolEntryCompleted, Tool: &ToolEntry{Name: "control_fork"}},
		{ID: "t3", Type: SessionEntryTool, State: ToolEntryPending, Tool: &ToolEntry{Name: "react"}},
		{ID: "t4", Type: SessionEntryTool, State: ToolEntryPending, Tool: &ToolEntry{Name: "ask_user"}},
	}
	if got := controlSettleEntry(entries); got == nil || got.ID != "t4" {
		t.Fatalf("settle entry = %#v", got)
	}
	if got := controlSettleEntry(entries[:3]); got != nil {
		t.Fatalf("settle entry = %#v", got)
	}
}

func TestTruncateControlTextKeepsRunesAndCap(t *testing.T) {
	short := "fine"
	if truncateControlText(short, "run-1") != short {
		t.Fatal("short text changed")
	}
	long := strings.Repeat("é", maximumControlTextBytes)
	got := truncateControlText(long, "run-1")
	if len(got) > maximumControlTextBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "…[truncated; full result is in run run-1]") {
		t.Fatalf("truncated text: len %d valid %v suffix %q", len(got), utf8.ValidString(got), got[len(got)-50:])
	}
}

func TestControlJSONResultIsCompactWithStatus(t *testing.T) {
	result := controlJSONResult(ToolResultSucceeded, map[string]interface{}{"taskId": "t1"})
	if result.IsError || string(result.Content[0].Value) != `{"status":"succeeded","taskId":"t1"}` {
		t.Fatalf("result = %s", result.Content[0].Value)
	}
	result = controlJSONResult(ToolResultSucceeded, map[string]interface{}{"status": "started"})
	if string(result.Content[0].Value) != `{"status":"started"}` {
		t.Fatalf("result = %s", result.Content[0].Value)
	}
	refused := controlErrorResult(ToolResultSucceeded, "x", "Nope.", map[string]interface{}{"error": "ignored"})
	requireControlError(t, refused, ToolResultFailed, "x")
}
