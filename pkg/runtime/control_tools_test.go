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

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
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

func (h *fakeControlHandler) Spec(a agentrun.ControlAuthority) ControlToolSpec {
	return ControlToolSpec{Description: "fake", InputSchema: map[string]interface{}{"type": "object"}}
}

func (h *fakeControlHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return models.DecodeControlInto[fakeControlArgs](raw)
}

func (h *fakeControlHandler) Run(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResult, error) {
	h.calls++
	h.seen = append(h.seen, cc.Arguments)
	return x.complete(ctx, cc, models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{"status": "done"}), &ControlEntryMeta{ForkID: "fork-" + cc.Entry.ID})
}

func fakeControlRegistry() (Registry, map[models.ControlToolName]*fakeControlHandler) {
	registry := Registry{}
	handlers := map[models.ControlToolName]*fakeControlHandler{}
	for _, name := range controlToolOrder {
		h := &fakeControlHandler{}
		handlers[name] = h
		registry.register(name, h)
	}
	return registry, handlers
}

// builtinControlSpecs are the specs of the built-in control tools offered
// under a. The runbook tools are never offered, so they are the specs every
// loop step gets.
func builtinControlSpecs(a agentrun.ControlAuthority) []ControlToolSpec {
	return NewRegistry(BuiltinTools()...).Specs(a)
}

// builtinControlModelStepTools are builtinControlSpecs as model step tools.
func builtinControlModelStepTools(a agentrun.ControlAuthority) []models.ModelStepTool {
	specs := builtinControlSpecs(a)
	tools := make([]models.ModelStepTool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, spec.ModelStepTool())
	}
	return tools
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
	everything := agentrun.ControlAuthority{
		CanReportProgress: true, CanWait: true, CanAskUser: true, CanReact: true,
		TaskStart:       &models.HostedConversationTaskContext{ConversationID: "c1", CanStart: true},
		ForkAllowed:     true,
		Operations:      []agentrun.HostedRunbookOperation{{Name: "op"}},
		DelegateTargets: []agentrun.HostedAgentTarget{{ID: "a2", DisplayName: "Other"}},
	}
	cases := []struct {
		name string
		edit func(*agentrun.ControlAuthority)
		want []string
	}{
		{"everything", func(*agentrun.ControlAuthority) {}, []string{
			"turn_final_answer", "turn_report_progress", "wait", "ask_user", "react", "conversation_start_task",
			"control_fork", "run_operation", "agent_delegate", "agent_delegate_reply",
		}},
		{"zero authority offers only the final answer", func(a *agentrun.ControlAuthority) { *a = agentrun.ControlAuthority{} }, []string{"turn_final_answer"}},
		{"spoken and evidence keep the default final tool", func(a *agentrun.ControlAuthority) {
			*a = agentrun.ControlAuthority{FinalMode: conversationrun.FinalModeSpoken, CanReact: true}
		}, []string{"turn_final_answer", "react"}},
		{"voice call hides task start", func(a *agentrun.ControlAuthority) { a.VoiceCallStarted = true }, []string{
			"turn_final_answer", "turn_report_progress", "wait", "ask_user", "react",
			"control_fork", "run_operation", "agent_delegate", "agent_delegate_reply",
		}},
		{"task context that cannot start", func(a *agentrun.ControlAuthority) {
			a.TaskStart = &models.HostedConversationTaskContext{ConversationID: "c1"}
			a.Operations = nil
			a.DelegateTargets = nil
		}, []string{"turn_final_answer", "turn_report_progress", "wait", "ask_user", "react", "control_fork"}},
		{"decision Run offers only its tool", func(a *agentrun.ControlAuthority) { a.FinalMode = agentrun.FinalModeDecision }, []string{"agent_request_decision"}},
		{"review Run offers only its tool", func(a *agentrun.ControlAuthority) { a.FinalMode = agentrun.FinalModeReview }, []string{"agent_request_review"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authority := everything
			tc.edit(&authority)
			specs := registry.Specs(authority)
			if got := specNames(specs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("specs = %v, want %v", got, tc.want)
			}
			// The list is a pure function of the authority (prompt cache).
			if again := specNames(registry.Specs(authority)); !reflect.DeepEqual(again, tc.want) {
				t.Fatalf("second listing = %v", again)
			}
			for _, spec := range specs {
				if spec.Phase != controlToolRules[spec.Name].Phase {
					t.Fatalf("%s phase = %s", spec.Name, spec.Phase)
				}
				tool := spec.ModelStepTool()
				if tool.Group != models.ModelStepToolHarness || tool.Name != string(spec.Name) {
					t.Fatalf("model step tool = %#v", tool)
				}
			}
		})
	}
	// Unregistered tools are never offered.
	partial := Registry{}
	partial.register(models.ControlToolWait, &fakeControlHandler{})
	if got := specNames(partial.Specs(everything)); !reflect.DeepEqual(got, []string{"wait"}) {
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

func modelCalls(names ...string) []models.ModelToolCall {
	calls := make([]models.ModelToolCall, 0, len(names))
	for i, name := range names {
		calls = append(calls, models.ModelToolCall{ID: "call-" + string(rune('a'+i)), Name: name, Arguments: json.RawMessage(`{}`)})
	}
	return calls
}

func requireControlError(t *testing.T, result *models.ToolResult, status models.ToolResultStatus, code string) map[string]interface{} {
	t.Helper()
	if result == nil || !result.IsError || result.Status != status || len(result.Content) != 1 || result.Content[0].Type != models.ContentPartJSON {
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
	refusals := CallRefusals(calls, allSettleTools)
	if len(refusals) != 2 || refusals["call-a"] != nil || refusals["call-b"] != nil || refusals["call-d"] != nil {
		t.Fatalf("refusals = %v", refusals)
	}
	value := requireControlError(t, refusals["call-c"], models.ToolResultRefused, "control_one_settle_call")
	if value["message"] != "Call ask_user alone in its own message after this one returns." {
		t.Fatalf("message = %v", value["message"])
	}
	requireControlError(t, refusals["call-e"], models.ToolResultRefused, "control_one_settle_call")
	if got := controlSettleConflicts(calls, allSettleTools); !reflect.DeepEqual(got, []int{2, 4}) {
		t.Fatalf("conflicts = %v", got)
	}
	// One settle call among inline and governed calls is fine.
	if refusals := CallRefusals(modelCalls("react", "control_fork", "linear_get_issue"), allSettleTools); len(refusals) != 0 {
		t.Fatalf("single settle call refused: %v", refusals)
	}
}

func TestFinalAnswerWithOtherCallsIsRefusedOthersRun(t *testing.T) {
	refusals := CallRefusals(modelCalls("linear_get_issue", "turn_final_answer", "react"), allSettleTools)
	if len(refusals) != 1 {
		t.Fatalf("refusals = %v", refusals)
	}
	value := requireControlError(t, refusals["call-b"], models.ToolResultRefused, "final_answer_not_alone")
	if value["message"] != "Call turn_final_answer alone after your other tools have returned." {
		t.Fatalf("message = %v", value["message"])
	}
	// A refused final does not take the settle slot: the fork still settles.
	refusals = CallRefusals(modelCalls("turn_final_answer", "control_fork"), allSettleTools)
	if len(refusals) != 1 || refusals["call-b"] != nil {
		t.Fatalf("refusals = %v", refusals)
	}
	requireControlError(t, refusals["call-a"], models.ToolResultRefused, "final_answer_not_alone")
	// Task start must also be alone (A4).
	refusals = CallRefusals(modelCalls("conversation_start_task", "react"), allSettleTools)
	if len(refusals) != 1 {
		t.Fatalf("refusals = %v", refusals)
	}
	value = requireControlError(t, refusals["call-a"], models.ToolResultRefused, "task_not_alone")
	if value["message"] != "Call conversation_start_task alone; other work belongs inside the task." {
		t.Fatalf("message = %v", value["message"])
	}
	// Alone, the final answer settles.
	if refusals := CallRefusals(modelCalls("turn_final_answer"), allSettleTools); len(refusals) != 0 {
		t.Fatalf("lone final refused: %v", refusals)
	}
}

// controlFixture is a claimed Run with a session holding one finished step
// whose assistant entry made the given calls (all entries pending).
type controlFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *sqlite.Store
	records  *controlRecords
	run      *models.AgentRun
	session  *models.AgentSession
	step     *models.AgentTurn
	entries  map[string]*models.SessionEntry
	exec     *ControlExecutor
	handlers map[models.ControlToolName]*fakeControlHandler
	now      time.Time
}

const controlTestWorker = "control-worker"

func newControlFixture(t *testing.T, calls []agentrun.LoopToolCall) *controlFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "control-" + uuid.NewString()[:8]}
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}, AssignedAgentID: "agent-1",
		Goal: "work", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: scope, WorkerID: controlTestWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	opened, err := store.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{
		Scope: scope, OwnerRunID: run.ID, AgentID: "agent-1",
		Lease: models.AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: now}, Now: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	stepID := "step-1"
	assistant := &models.SessionEntry{
		ID: models.SessionEntryID(opened.Session.ID, models.AssistantEntryKey(stepID)), RunID: run.ID, StepID: stepID, Step: 1,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
		},
	}
	batch := []*models.SessionEntry{assistant}
	for _, call := range calls {
		entryStep := stepID
		if call.Directive == nil {
			assistant.Assistant.ToolCalls = append(assistant.Assistant.ToolCalls, models.ModelToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		} else {
			// Deterministic calls are appended without an assistant entry,
			// under their own step.
			entryStep = "step-kernel"
		}
		entry := &models.SessionEntry{
			ID: models.SessionEntryID(opened.Session.ID, models.ToolEntryKey(entryStep, call.ID)), RunID: run.ID, StepID: entryStep, Step: 1,
			Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: call.ID,
			Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: models.ToolSideEffectWrite},
		}
		if call.Directive != nil {
			entry.Tool.Directive, _ = json.Marshal(call.Directive)
		}
		batch = append(batch, entry)
	}
	appended, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: scope, SessionID: opened.Session.ID, RunID: run.ID,
		Lease: &models.AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: now}, Entries: batch, Now: now,
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
		step:    &models.AgentTurn{ID: stepID, RunID: run.ID, Sequence: 1, Status: models.AgentTurnStatusCompleted},
		entries: map[string]*models.SessionEntry{},
	}
	for _, entry := range appended.Entries {
		if entry.Type == models.SessionEntryTool {
			f.entries[entry.ToolCallID] = entry
		}
	}
	registry, handlers := fakeControlRegistry()
	f.handlers = handlers
	f.records = &controlRecords{Store: store}
	f.exec = &ControlExecutor{Portfolio: f.records, Sessions: store, Now: func() time.Time { return now }, handlers: registry}
	return f
}

// controlRecords is the executor's portfolio: the fixture's store, plus the
// action calls and conversation tasks a test states for its Runs. The control
// tools only list a Run's action calls and find a task by its work Run, so a
// test gives those records here instead of driving them through the action
// and task workflows. The store itself holds no action calls or tasks.
type controlRecords struct {
	*sqlite.Store
	actions controlActionCallRecords
	tasks   []*models.ConversationTask
}

// putAction stores call as given, replacing the call with the same ID.
func (f *controlFixture) putAction(call *models.ActionCall) { f.records.actions.put(call) }

// action returns a copy of a call the test stored.
func (f *controlFixture) action(id string) *models.ActionCall { return f.records.actions.get(f.t, id) }

// putConversationTask stores task as given, as the task of its work Run.
func (f *controlFixture) putConversationTask(task *models.ConversationTask) {
	f.records.tasks = append(f.records.tasks, models.CloneConversationTask(task))
}

func (s *controlRecords) ListActionCalls(ctx context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
	return s.actions.ListActionCalls(ctx, filter)
}

// FindConversationTaskByWorkRunID finds a stored task by its work Run.
func (s *controlRecords) FindConversationTaskByWorkRunID(_ context.Context, scope models.Scope, runID string) (*models.ConversationTask, error) {
	for _, task := range s.tasks {
		if task.Scope == scope && task.WorkRunID == runID {
			return models.CloneConversationTask(task), nil
		}
	}
	return nil, nil
}

func (f *controlFixture) context(callID string, authority agentrun.ControlAuthority) agentrun.ControlContext {
	entry := f.entries[callID]
	if entry == nil {
		f.t.Fatalf("no entry for call %s", callID)
	}
	step := f.step
	if entry.StepID != step.ID {
		// Deterministic entries carry their own (finished, applied) step.
		step = &models.AgentTurn{ID: entry.StepID, RunID: f.run.ID, Sequence: f.step.Sequence, Status: models.AgentTurnStatusCompleted}
	}
	return NewControlContext(agentrun.StepToolsInput{
		Run: f.run, Step: step, Session: f.session, WorkerID: controlTestWorker,
		Binding: &agentrun.TurnRunnerBinding{Authority: authority},
	}, entry)
}

func (f *controlFixture) stored(callID string) *models.SessionEntry {
	f.t.Helper()
	entry, err := f.store.GetSessionEntry(f.ctx, f.run.Scope, f.entries[callID].ID)
	if err != nil {
		f.t.Fatalf("get entry: %v", err)
	}
	return entry
}

func TestControlArgumentsRejectUnknownFields(t *testing.T) {
	f := newControlFixture(t, []agentrun.LoopToolCall{
		{ID: "unknown", Name: "control_fork", Arguments: json.RawMessage(`{"goal":"x","budget":3}`)},
		{ID: "string", Name: "wait", Arguments: json.RawMessage(`{"goal":3}`)},
		{ID: "ok", Name: "ask_user", Arguments: json.RawMessage(`{"goal":"x"}`)},
	})
	authority := agentrun.ControlAuthority{ForkAllowed: true, CanWait: true, CanAskUser: true}
	for _, callID := range []string{"unknown", "string"} {
		result, err := f.exec.Settle(f.ctx, f.context(callID, authority))
		if err != nil {
			t.Fatalf("settle %s: %v", callID, err)
		}
		if result.Kind != ControlContinue || result.Entry.State != models.ToolEntryError {
			t.Fatalf("%s result = %#v", callID, result)
		}
		requireControlError(t, f.stored(callID).Tool.Result, models.ToolResultFailed, "tool_arguments_invalid")
	}
	if f.handlers[models.ControlToolFork].calls != 0 || f.handlers[models.ControlToolWait].calls != 0 {
		t.Fatal("a handler ran with invalid arguments")
	}
	result, err := f.exec.Settle(f.ctx, f.context("ok", authority))
	if err != nil {
		t.Fatalf("settle ok: %v", err)
	}
	if result.Kind != ControlContinue || result.Entry.State != models.ToolEntryCompleted {
		t.Fatalf("ok result = %#v", result)
	}
	if args, _ := f.handlers[models.ControlToolAskUser].seen[0].(*fakeControlArgs); args == nil || args.Goal != "x" {
		t.Fatalf("decoded arguments = %#v", f.handlers[models.ControlToolAskUser].seen)
	}
	// One value only: a trailing value is refused (stored entries always hold
	// one object, so this is checked on the decoder).
	if _, err := models.DecodeControlInto[fakeControlArgs](json.RawMessage(`{"goal":"x"} {"goal":"y"}`)); err == nil {
		t.Fatal("trailing value accepted")
	}
	if args, err := models.DecodeControlInto[fakeControlArgs](nil); err != nil || args.(*fakeControlArgs).Goal != "" {
		t.Fatalf("empty arguments = %#v, %v", args, err)
	}
	meta, err := DecodeControlEntryMeta(f.stored("ok"))
	if err != nil || meta == nil || meta.Kind != models.ControlToolAskUser || meta.ForkID != "fork-"+f.entries["ok"].ID {
		t.Fatalf("meta = %#v, %v", meta, err)
	}
}

func TestControlToolNotOfferedIsError(t *testing.T) {
	f := newControlFixture(t, []agentrun.LoopToolCall{
		{ID: "fork", Name: "control_fork", Arguments: json.RawMessage(`{"goal":"x"}`)},
		{ID: "decide", Name: "agent_request_decision", Arguments: json.RawMessage(`{}`)},
		{ID: "kernel", Name: "agent_delegate", Arguments: json.RawMessage(`{"goal":"x"}`),
			Directive: &agentrun.ToolCallDirective{IdempotencyKey: "runbook-step"}},
	})
	authority := agentrun.ControlAuthority{CanWait: true}
	for _, callID := range []string{"fork", "decide"} {
		result, err := f.exec.Settle(f.ctx, f.context(callID, authority))
		if err != nil {
			t.Fatalf("settle %s: %v", callID, err)
		}
		if result.Kind != ControlContinue || result.Entry.State != models.ToolEntryError {
			t.Fatalf("%s result = %#v", callID, result)
		}
		value := requireControlError(t, f.stored(callID).Tool.Result, models.ToolResultRefused, "tool_not_available")
		if value["tool"] != f.entries[callID].Tool.Name {
			t.Fatalf("tool = %v", value["tool"])
		}
	}
	if f.handlers[models.ControlToolFork].calls != 0 || f.handlers[models.ControlToolRequestDecision].calls != 0 {
		t.Fatal("a handler ran for a tool that was not offered")
	}
	// A kernel-origin call (runbook directive) is preauthorized.
	cc := f.context("kernel", authority)
	if cc.Origin != ControlOriginKernel {
		t.Fatalf("origin = %s", cc.Origin)
	}
	result, err := f.exec.Settle(f.ctx, cc)
	if err != nil || result.Entry.State != models.ToolEntryCompleted || f.handlers[models.ControlToolDelegate].calls != 1 {
		t.Fatalf("kernel call = %#v, %v", result, err)
	}
}

func TestControlDispatcherPhaseAndStateRules(t *testing.T) {
	f := newControlFixture(t, []agentrun.LoopToolCall{
		{ID: "react", Name: "react", Arguments: json.RawMessage(`{}`)},
		{ID: "wait", Name: "wait", Arguments: json.RawMessage(`{}`)},
		{ID: "gov", Name: "linear_get_issue", Arguments: json.RawMessage(`{}`)},
	})
	authority := agentrun.ControlAuthority{CanReact: true, CanWait: true}
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
	cc.Step = &models.AgentTurn{ID: "step-1", Sequence: 1, Status: models.AgentTurnStatusRunning}
	if _, err := f.exec.Settle(f.ctx, cc); !errors.Is(err, ErrControlEntryState) {
		t.Fatalf("settle on running step = %v", err)
	}
	// The step must be the entry's own step.
	cc = f.context("wait", authority)
	cc.Step = &models.AgentTurn{ID: "step-0", Sequence: 1, Status: models.AgentTurnStatusCompleted}
	if _, err := f.exec.Settle(f.ctx, cc); !errors.Is(err, ErrControlEntryState) {
		t.Fatalf("settle under another step = %v", err)
	}
	// A step not yet applied cannot settle.
	cc = f.context("wait", authority)
	cc.Run = cloneRunForTest(f.run)
	cc.Run.LastAppliedTurn = 0
	if _, err := f.exec.Settle(f.ctx, cc); !errors.Is(err, ErrControlEntryState) {
		t.Fatalf("settle before apply = %v", err)
	}
	// A lost lease aborts the claim.
	cc = f.context("wait", authority)
	cc.WorkerID = "other"
	if _, err := f.exec.Settle(f.ctx, cc); !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("settle under another worker = %v", err)
	}
	// Inline runs at any time in the step.
	result, err := f.exec.Inline(f.ctx, f.context("react", authority))
	if err != nil || result.Entry.State != models.ToolEntryCompleted {
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
	entries := []*models.SessionEntry{
		storetest.ToolSessionEntry("t1", "s1", "c1", models.ToolEntryPending),
		{ID: "t2", Type: models.SessionEntryTool, State: models.ToolEntryCompleted, Tool: &models.ToolEntry{Name: "control_fork"}},
		{ID: "t3", Type: models.SessionEntryTool, State: models.ToolEntryPending, Tool: &models.ToolEntry{Name: "react"}},
		{ID: "t4", Type: models.SessionEntryTool, State: models.ToolEntryPending, Tool: &models.ToolEntry{Name: "ask_user"}},
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
	result := models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{"taskId": "t1"})
	if result.IsError || string(result.Content[0].Value) != `{"status":"succeeded","taskId":"t1"}` {
		t.Fatalf("result = %s", result.Content[0].Value)
	}
	result = models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{"status": "started"})
	if string(result.Content[0].Value) != `{"status":"started"}` {
		t.Fatalf("result = %s", result.Content[0].Value)
	}
	refused := ErrorResult(models.ToolResultSucceeded, "x", "Nope.", map[string]interface{}{"error": "ignored"})
	requireControlError(t, refused, models.ToolResultFailed, "x")
}

// allSettleTools offers every tool the phase-rule tests use.
var allSettleTools = agentrun.ControlAuthority{
	ForkAllowed: true, CanWait: true, CanAskUser: true, CanReact: true,
	TaskStart: &models.HostedConversationTaskContext{CanStart: true},
}

func cloneRunForTest(run *models.AgentRun) *models.AgentRun {
	clone := *run
	return &clone
}

func TestControlNotOfferedCallDoesNotTakeSettleSlot(t *testing.T) {
	calls := modelCalls("wait", "control_fork")
	refusals := CallRefusals(calls, agentrun.ControlAuthority{ForkAllowed: true})
	if len(refusals) != 0 {
		t.Fatalf("refusals = %v", refusals)
	}
	refusals = CallRefusals(calls, agentrun.ControlAuthority{ForkAllowed: true, CanWait: true})
	requireControlError(t, refusals["call-b"], models.ToolResultRefused, "control_one_settle_call")
}

func TestControlSettleOfEarlierAppliedStep(t *testing.T) {
	f := newControlFixture(t, []agentrun.LoopToolCall{{ID: "wait", Name: "wait", Arguments: json.RawMessage(`{}`)}})
	// The Run has since applied step 2; ResumeOpenTools settles the entry
	// left open by step 1 with step 1 as cc.Step.
	f.run.LastAppliedTurn = 2
	result, err := f.exec.Settle(f.ctx, f.context("wait", agentrun.ControlAuthority{CanWait: true}))
	if err != nil || result.Entry.State != models.ToolEntryCompleted {
		t.Fatalf("settle = %#v, %v", result, err)
	}
}

func TestControlTransitionRetriesOneStaleRevision(t *testing.T) {
	f := newControlFixture(t, []agentrun.LoopToolCall{{ID: "wait", Name: "wait", Arguments: json.RawMessage(`{}`)}})
	authority := agentrun.ControlAuthority{CanWait: true}
	cc := f.context("wait", authority)
	// Another writer bumps the entry's revision and leaves it pending again.
	bumped := cc.Entry
	for _, step := range [][2]models.ToolEntryState{{models.ToolEntryPending, models.ToolEntryRunning}, {models.ToolEntryRunning, models.ToolEntryPending}} {
		moved, err := f.store.TransitionToolEntry(f.ctx, models.ToolEntryTransitionRequest{
			Scope: f.run.Scope, EntryID: bumped.ID, RunID: f.run.ID,
			Lease: models.AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: f.now},
			From:  step[0], ExpectedRevision: bumped.Revision, To: step[1], Now: f.now,
		})
		if err != nil {
			t.Fatalf("bump %v: %v", step, err)
		}
		bumped = moved.Entry
	}
	if bumped.State != models.ToolEntryPending || bumped.Revision == cc.Entry.Revision {
		t.Fatal("revision not bumped")
	}
	meta := &ControlEntryMeta{}
	entry, err := f.exec.transitionControlEntry(f.ctx, cc, models.ToolEntryCompleted, nil,
		models.ControlJSONResult(models.ToolResultSucceeded, nil), meta)
	if err != nil || entry.State != models.ToolEntryCompleted {
		t.Fatalf("transition = %#v, %v", entry, err)
	}
	if meta.Kind != "" {
		t.Fatalf("caller meta changed: %#v", meta)
	}
	stored, err := DecodeControlEntryMeta(f.stored("wait"))
	if err != nil || stored == nil || stored.Kind != models.ControlToolWait {
		t.Fatalf("stored meta = %#v, %v", stored, err)
	}
	// A state change by another writer is not retried.
	f2 := newControlFixture(t, []agentrun.LoopToolCall{{ID: "wait", Name: "wait", Arguments: json.RawMessage(`{}`)}})
	cc2 := f2.context("wait", authority)
	if _, err := f2.store.TransitionToolEntry(f2.ctx, models.ToolEntryTransitionRequest{
		Scope: f2.run.Scope, EntryID: cc2.Entry.ID, RunID: f2.run.ID,
		Lease: models.AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: f2.now},
		From:  models.ToolEntryPending, ExpectedRevision: cc2.Entry.Revision, To: models.ToolEntryRunning, Now: f2.now,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f2.exec.transitionControlEntry(f2.ctx, cc2, models.ToolEntryCompleted, nil,
		models.ControlJSONResult(models.ToolResultSucceeded, nil), nil); !errors.Is(err, models.ErrToolEntryStateConflict) {
		t.Fatalf("transition after state change = %v", err)
	}
}
