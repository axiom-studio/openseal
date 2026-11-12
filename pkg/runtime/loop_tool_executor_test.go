package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/workspace"
	"github.com/google/uuid"
)

const toolExecWorker = "tool-exec-worker"

// loopFakeControlHandler completes its control entry with a fixed result, as
// a control tool does, and counts its calls.
type loopFakeControlHandler struct {
	calls int
}

func (h *loopFakeControlHandler) Spec(agentrun.ControlAuthority) control.ControlToolSpec {
	return control.ControlToolSpec{Description: "fake", InputSchema: map[string]interface{}{"type": "object"}}
}

func (h *loopFakeControlHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return models.DecodeControlInto[struct {
		Goal string `json:"goal"`
	}](raw)
}

func (h *loopFakeControlHandler) Run(ctx context.Context, x *control.ControlExecutor, cc agentrun.ControlContext) (control.ControlResult, error) {
	h.calls++
	result := models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{"status": "done"})
	meta, err := json.Marshal(&control.ControlEntryMeta{Kind: cc.ToolName(), ForkID: "fork-" + cc.Entry.ID})
	if err != nil {
		return control.ControlResult{}, err
	}
	now := x.Now()
	moved, err := x.Sessions.TransitionToolEntry(ctx, ToolEntryTransitionRequest{
		Scope: cc.Run.Scope, EntryID: cc.Entry.ID, RunID: cc.Run.ID, Lease: cc.LeaseGuard(now),
		From: cc.Entry.State, ExpectedRevision: cc.Entry.Revision, To: ToolEntryCompleted,
		Result: result, Kernel: &ToolEntryKernelPatch{Control: meta}, Now: now,
		Tokens: modelstep.EstimateEntryTokens(control.WithToolResult(cc.Entry, ToolEntryCompleted, result)),
	})
	if err != nil {
		return control.ControlResult{}, err
	}
	return control.ControlResult{Kind: control.ControlContinue, Entry: moved.Entry}, nil
}

// toolExecFixture is a claimed Run with a run session in an in-memory store and
// a claim tool executor whose host tools go to host and whose control tools
// go to fake handlers.
type toolExecFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *sqlite.Store
	scope    Scope
	now      time.Time
	run      *AgentRun
	host     *scriptedHost
	control  *control.ControlExecutor
	handlers map[ControlToolName]*loopFakeControlHandler
	binding  *agentrun.TurnRunnerBinding
}

// fakeLoopControls fakes every control tool of the run loop, except the
// tools in real, which keep the handler given there.
func fakeLoopControls(real ...control.Tool) (control.Registry, map[ControlToolName]*loopFakeControlHandler) {
	registry := control.NewRegistry(real...)
	fakes := map[ControlToolName]*loopFakeControlHandler{}
	for name := range loopControlRegistry() {
		if _, ok := registry[name]; !ok {
			fakes[name] = &loopFakeControlHandler{}
			registry[name] = fakes[name]
		}
	}
	return registry, fakes
}

// newToolExecFixture fakes the control tools, except the tools in real.
func newToolExecFixture(t *testing.T, authority agentrun.ControlAuthority, real ...control.Tool) *toolExecFixture {
	t.Helper()
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	scope := Scope{Kind: "tenant", ID: "tools-" + uuid.NewString()[:8]}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}, AssignedAgentID: "agent-1",
		Goal: "work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: toolExecWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	if _, err := store.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
		Scope: scope, OwnerRunID: run.ID, AgentID: "agent-1", Lease: AgentRunLeaseGuard{WorkerID: toolExecWorker, Now: now}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	f := &toolExecFixture{t: t, ctx: ctx, store: store, scope: scope, now: now, run: run, host: newScriptedHost()}
	f.run = f.currentRun()
	registry, handlers := fakeLoopControls(real...)
	f.handlers = handlers
	f.control = &control.ControlExecutor{Portfolio: store, Sessions: store, Now: func() time.Time { return now }}
	f.control.UseTools(registry)
	dispatcher, err := NewStepToolDispatcher(StepToolDispatcherConfig{
		Sessions: store, Turns: store, Actions: store, Control: f.control,
		Host: modelstep.NewHostToolDispatcher(f.host, nil, nil), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.binding = &agentrun.TurnRunnerBinding{Authority: authority}
	f.binding.Tools = dispatcher.ForClaim(LoopToolClaim{Workspace: workspace.ChatAuthority("chat-1")})
	return f
}

func (f *toolExecFixture) currentRun() *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.scope, f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// toolStep is one appended model step.
type toolStep struct {
	turn      *AgentTurn
	assistant *SessionEntry
	entries   []*SessionEntry
}

// appendToolStep creates the step row (status as given) and appends its
// assistant entry with the planned tool entries in one append.
func appendToolStep(t *testing.T, ctx context.Context, store interface {
	AgentSessionStore
	AgentTurnStore
}, run *AgentRun, binding *agentrun.TurnRunnerBinding, worker, stepID string, status AgentTurnStatus, now time.Time, calls ...ModelToolCall) toolStep {
	t.Helper()
	turn, err := store.CreateAgentTurn(ctx, &AgentTurn{
		ID: stepID, Scope: run.Scope, RunID: run.ID, Status: status, Producer: StepProducerModel, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create step %s: %v", stepID, err)
	}
	assistant := &SessionEntry{
		ID: SessionEntryID(run.SessionID, AssistantEntryKey(stepID)), Scope: run.Scope, SessionID: run.SessionID,
		RunID: run.ID, StepID: stepID, Step: turn.Sequence, Type: SessionEntryAssistant,
		Assistant: &AssistantEntry{Provider: ModelStepProviderAnthropic, Model: "claude-x", Text: "Working.", StopReason: ModelStopToolCalls, ToolCalls: calls},
	}
	entries, err := PlanToolEntries(binding, assistant)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	appended, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: run.SessionID, RunID: run.ID,
		Lease: &AgentRunLeaseGuard{WorkerID: worker, Now: now}, Entries: append([]*SessionEntry{assistant}, entries...), Now: now,
	})
	if err != nil {
		t.Fatalf("append step %s: %v", stepID, err)
	}
	return toolStep{turn: turn, assistant: appended.Entries[0], entries: appended.Entries[1:]}
}

func (f *toolExecFixture) step(stepID string, status AgentTurnStatus, calls ...ModelToolCall) toolStep {
	f.t.Helper()
	return appendToolStep(f.t, f.ctx, f.store, f.run, f.binding, toolExecWorker, stepID, status, f.now, calls...)
}

func (f *toolExecFixture) input(step toolStep) agentrun.StepToolsInput {
	return agentrun.StepToolsInput{
		Run: f.currentRun(), Step: step.turn, Binding: f.binding, WorkerID: toolExecWorker,
		Assistant: step.assistant, Entries: step.entries,
	}
}

func (f *toolExecFixture) entry(entry *SessionEntry) *SessionEntry {
	f.t.Helper()
	stored, err := f.store.GetSessionEntry(f.ctx, f.scope, entry.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return stored
}

func toolCall(id, name, args string) ModelToolCall {
	return ModelToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func resultValue(t *testing.T, entry *SessionEntry) map[string]interface{} {
	t.Helper()
	if entry.Tool.Result == nil || len(entry.Tool.Result.Content) == 0 || entry.Tool.Result.Content[0].Type != ContentPartJSON {
		t.Fatalf("entry %s result = %#v", entry.ToolCallID, entry.Tool.Result)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(entry.Tool.Result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// scriptedHost is a chat Workspace host: write_file stores, read_file reads,
// and every call can be held at a gate to observe concurrency.
type scriptedHost struct {
	mu        sync.Mutex
	files     map[string]string
	order     []string
	active    int
	maxActive int
	gate      chan struct{}
	gateOp    string // when set, only this operation waits at gate
}

func newScriptedHost() *scriptedHost { return &scriptedHost{files: map[string]string{}} }

func (h *scriptedHost) ExecuteHostTool(ctx context.Context, request HostToolRequest) (HostToolResult, error) {
	h.mu.Lock()
	h.active++
	if h.active > h.maxActive {
		h.maxActive = h.active
	}
	path, _ := request.Arguments["path"].(string)
	h.order = append(h.order, request.Operation+" "+path)
	gate := h.gate
	if h.gateOp != "" && h.gateOp != request.Operation {
		gate = nil
	}
	h.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active--
	var value map[string]interface{}
	switch request.Operation {
	case workspace.OperationWriteFile:
		h.files[path], _ = request.Arguments["content"].(string)
		value = map[string]interface{}{"path": path, "written": true}
	default:
		content, ok := h.files[path]
		value = map[string]interface{}{"path": path, "content": content, "exists": ok}
	}
	encoded, _ := json.Marshal(value)
	return HostToolResult{APIVersion: HostToolAPIVersion, InvocationID: request.InvocationID,
		Content: []ContentPart{{Type: ContentPartJSON, Value: encoded}}}, nil
}

func (h *scriptedHost) activeNow() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

func (h *scriptedHost) orderNow() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.order...)
}

func TestReadAfterWriteSeesWrite(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{})
	step := f.step("step-1", AgentTurnStatusRunning,
		toolCall("call-w", "workspace_write_file", `{"path":"notes.md","content":"fresh"}`),
		toolCall("call-r", "workspace_read_file", `{"path":"notes.md"}`),
	)
	if step.entries[0].Tool.SideEffect != ToolSideEffectWrite || step.entries[1].Tool.SideEffect != ToolSideEffectRead ||
		step.entries[0].Tool.RepeatEffect != string(RepeatEffectStateful) || step.entries[1].Tool.ArgumentsDigest == "" {
		t.Fatalf("planned = %#v / %#v", step.entries[0].Tool, step.entries[1].Tool)
	}
	// Hold the write: the read must not start until it finished.
	f.host.gate, f.host.gateOp = make(chan struct{}), workspace.OperationWriteFile
	done := make(chan struct{})
	var out agentrun.StepToolsOutcome
	var err error
	go func() {
		defer close(done)
		out, err = f.binding.Tools.ExecuteStepTools(f.ctx, f.input(step))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.host.activeNow() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the write never started")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if order := f.host.orderNow(); len(order) != 1 || order[0] != "workspace.write_file notes.md" {
		t.Fatalf("while the write is blocked the host saw %v", order)
	}
	close(f.host.gate)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if out.Park != nil || out.Failure != nil || out.SettlePending || out.HostToolCalls != 2 {
		t.Fatalf("outcome = %#v", out)
	}
	if strings.Join(f.host.order, ",") != "workspace.write_file notes.md,workspace.read_file notes.md" {
		t.Fatalf("host order = %v", f.host.order)
	}
	read := f.entry(step.entries[1])
	if read.State != ToolEntryCompleted || resultValue(t, read)["content"] != "fresh" || read.Tool.ResultDigest == "" {
		t.Fatalf("read = %s %#v", read.State, read.Tool.Result)
	}
	if write := f.entry(step.entries[0]); write.State != ToolEntryCompleted {
		t.Fatalf("write = %s", write.State)
	}
}

func TestReadsRunConcurrentlyBound4(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{})
	f.host.gate = make(chan struct{})
	var calls []ModelToolCall
	for i := 0; i < 6; i++ {
		calls = append(calls, toolCall(fmt.Sprintf("call-%d", i), "workspace_read_file", fmt.Sprintf(`{"path":"f%d.md"}`, i)))
	}
	step := f.step("step-1", AgentTurnStatusRunning, calls...)
	done := make(chan struct{})
	var out agentrun.StepToolsOutcome
	var err error
	go func() {
		defer close(done)
		out, err = f.binding.Tools.ExecuteStepTools(f.ctx, f.input(step))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.host.activeNow() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d reads started concurrently", f.host.activeNow())
		}
		time.Sleep(time.Millisecond)
	}
	// Give a fifth read the chance to start if the bound were broken.
	time.Sleep(20 * time.Millisecond)
	if active := f.host.activeNow(); active != 4 {
		t.Fatalf("%d reads ran at once, want 4", active)
	}
	close(f.host.gate)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if out.HostToolCalls != 6 || f.host.maxActive != 4 {
		t.Fatalf("host calls = %d, max concurrency = %d", out.HostToolCalls, f.host.maxActive)
	}
	for _, entry := range step.entries {
		if stored := f.entry(entry); stored.State != ToolEntryCompleted {
			t.Fatalf("%s = %s", entry.ToolCallID, stored.State)
		}
	}
}

func TestSecondSettleCallRefused(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{CanWait: true, CanAskUser: true})
	step := f.step("step-1", AgentTurnStatusRunning,
		toolCall("call-wait", "wait", `{"goal":"a"}`),
		toolCall("call-ask", "ask_user", `{"goal":"b"}`),
	)
	out, err := f.binding.Tools.ExecuteStepTools(f.ctx, f.input(step))
	if err != nil {
		t.Fatal(err)
	}
	if !out.SettlePending || out.Park != nil || out.Failure != nil {
		t.Fatalf("outcome = %#v", out)
	}
	ask := f.entry(step.entries[1])
	if ask.State != ToolEntryError || resultValue(t, ask)["error"] != "control_one_settle_call" {
		t.Fatalf("second settle call = %s %v", ask.State, ask.Tool.Result)
	}
	if wait := f.entry(step.entries[0]); wait.State != ToolEntryPending || f.handlers[ControlToolWait].calls != 0 {
		t.Fatalf("first settle call ran in the inline phase: %s", wait.State)
	}
	// Phase 10: the step is finished and applied, then the one call settles.
	finished := *step.turn
	finished.Status = AgentTurnStatusCompleted
	in := f.input(step)
	in.Step = &finished
	in.Run.LastAppliedTurn = finished.Sequence
	settled, err := f.binding.Tools.SettleStep(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Failure != nil || settled.Park != nil || f.handlers[ControlToolWait].calls != 1 || f.handlers[ControlToolAskUser].calls != 0 {
		t.Fatalf("settle = %#v, wait calls %d, ask calls %d", settled, f.handlers[ControlToolWait].calls, f.handlers[ControlToolAskUser].calls)
	}
	if wait := f.entry(step.entries[0]); wait.State != ToolEntryCompleted {
		t.Fatalf("settled call = %s", wait.State)
	}
}

func TestUnofferedToolIsError(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{})
	step := f.step("step-1", AgentTurnStatusRunning,
		toolCall("call-unknown", "made_up_tool", `{}`),
		toolCall("call-fork", "control_fork", `{"goal":"x"}`),
		toolCall("call-load", "load_capabilities", `{"ids":["action:x"]}`),
		toolCall("call-read", "workspace_read_file", `{"path":"a.md"}`),
	)
	out, err := f.binding.Tools.ExecuteStepTools(f.ctx, f.input(step))
	if err != nil {
		t.Fatal(err)
	}
	if out.SettlePending || out.Park != nil || out.Failure != nil {
		t.Fatalf("outcome = %#v", out)
	}
	for _, entry := range step.entries[:2] {
		stored := f.entry(entry)
		if stored.State != ToolEntryError || !stored.Tool.Result.IsError || resultValue(t, stored)["error"] != loopToolNotAvailableCode {
			t.Fatalf("%s = %s %v", entry.ToolCallID, stored.State, stored.Tool.Result)
		}
	}
	// Loading is offered (always deferred); an ID outside the catalog is
	// refused.
	if stored := f.entry(step.entries[2]); !stored.Tool.Result.IsError || resultValue(t, stored)["status"] != "not_available" {
		t.Fatalf("load of an unknown ID = %s %v", stored.State, stored.Tool.Result)
	}
	if f.handlers[ControlToolFork].calls != 0 {
		t.Fatal("an unoffered control tool ran")
	}
	if read := f.entry(step.entries[3]); read.State != ToolEntryCompleted {
		t.Fatalf("offered read = %s", read.State)
	}
}

func TestPlanToolEntriesRefusesUnusableArguments(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{})
	call := toolCall("call-bad", "workspace_read_file", `{}`)
	call.ArgumentsError = "unexpected end of JSON input"
	step := f.step("step-1", AgentTurnStatusRunning, call)
	entry := step.entries[0]
	if entry.State != ToolEntryError || entry.Tool.Result.Status != ToolResultRefused || resultValue(t, entry)["error"] != control.ArgumentsInvalidCode {
		t.Fatalf("entry = %s %#v", entry.State, entry.Tool.Result)
	}
	out, err := f.binding.Tools.ExecuteStepTools(f.ctx, f.input(step))
	if err != nil || out.HostToolCalls != 0 {
		t.Fatalf("outcome = %#v, %v", out, err)
	}
}

// committedForkHandler plays a fork coordinator: its settle creates the
// dependency group under the entry's stable key (once) and moves the entry to
// awaiting_child, as the coordinator transaction does.
type committedForkHandler struct {
	loopFakeControlHandler
	groups  map[string]int
	settles int
}

func (h *committedForkHandler) Run(ctx context.Context, x *control.ControlExecutor, cc agentrun.ControlContext) (control.ControlResult, error) {
	h.settles++
	key := "fork-" + cc.Entry.ID
	h.groups[key]++
	meta, err := json.Marshal(&control.ControlEntryMeta{Kind: ControlToolFork, ForkID: key, GroupID: key})
	if err != nil {
		return control.ControlResult{}, err
	}
	now := x.Now()
	moved, err := x.Sessions.TransitionToolEntry(ctx, ToolEntryTransitionRequest{
		Scope: cc.Run.Scope, EntryID: cc.Entry.ID, RunID: cc.Run.ID, Lease: AgentRunLeaseGuard{WorkerID: cc.WorkerID, Now: now},
		From: cc.Entry.State, ExpectedRevision: cc.Entry.Revision, To: ToolEntryAwaitingChild,
		Wait: &ToolEntryWait{GroupID: key}, Kernel: &ToolEntryKernelPatch{Control: meta}, Now: now,
	})
	if err != nil {
		return control.ControlResult{}, err
	}
	entry := moved.Entry
	return control.ControlResult{Kind: control.ControlParked, Entry: entry, Park: &agentrun.LoopPark{
		Status: AgentRunStatusWaitingForDependency, Wake: &WakeCondition{Type: "run_dependencies", Reference: key},
		Summary: "Waiting for the branches", Committed: true,
	}}, nil
}

// TestResumeControlEntryWithoutGroupResettles covers a settle-phase control
// entry left pending (the group was never created): the resume settles it
// again. An awaiting_child entry whose group/request is missing goes to
// ControlExecutor.Resume, whose resumers own that row of SPEC §3.7 (CTL-5b,
// CTL-6b).
func TestResumeControlEntryWithoutGroupResettles(t *testing.T) {
	fork := &committedForkHandler{groups: map[string]int{}}
	f := newToolExecFixture(t, agentrun.ControlAuthority{ForkAllowed: true}, control.NewTool(ControlToolFork, fork))
	// Step 1 asked for a fork; the step finished and was applied, then the
	// worker died before the settle phase created the group.
	step := f.step("step-1", AgentTurnStatusCompleted, toolCall("call-fork", "control_fork", `{"goal":"split"}`))
	if step.entries[0].Tool.RepeatEffect != string(RepeatEffectWrite) {
		t.Fatalf("fork repeat effect = %q", step.entries[0].Tool.RepeatEffect)
	}
	if len(fork.groups) != 0 {
		t.Fatal("the group exists before the resume")
	}
	// The next claim's step resumes the open entry before its model call.
	next, err := f.store.CreateAgentTurn(f.ctx, &AgentTurn{
		ID: "step-2", Scope: f.scope, RunID: f.run.ID, Status: AgentTurnStatusRunning, Producer: StepProducerModel, Revision: 1,
		CreatedAt: f.now, UpdatedAt: f.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := f.currentRun()
	run.LastAppliedTurn = step.turn.Sequence
	out, err := f.binding.Tools.ResumeOpenTools(f.ctx, agentrun.StepToolsInput{Run: run, Step: next, Binding: f.binding, WorkerID: toolExecWorker})
	if err != nil {
		t.Fatal(err)
	}
	if fork.settles != 1 || fork.groups["fork-"+step.entries[0].ID] != 1 {
		t.Fatalf("settles = %d, groups = %v", fork.settles, fork.groups)
	}
	if out.Park == nil || !out.Park.Committed || out.Park.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("outcome = %#v", out)
	}
	stored := f.entry(step.entries[0])
	if stored.State != ToolEntryAwaitingChild || stored.Tool.Wait == nil || stored.Tool.Wait.GroupID != "fork-"+step.entries[0].ID {
		t.Fatalf("entry = %s %#v", stored.State, stored.Tool.Wait)
	}
}

// governedToolExecutor wires a claim tool executor over a governed fixture.
func governedToolExecutor(t *testing.T, f *governedFixture) (agentrun.LoopToolExecutor, AgentSessionStore) {
	t.Helper()
	sessions, ok := f.store.(AgentSessionStore)
	if !ok {
		t.Fatalf("%T is not a session store", f.store)
	}
	dispatcher, err := NewStepToolDispatcher(StepToolDispatcherConfig{
		Sessions: sessions, Turns: f.store, Actions: f.store, Coordinator: f.actions, Governed: f.executor,
		Now: func() time.Time { return f.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.CreateRunAgentSession(f.ctx, CreateRunAgentSessionRequest{
		Scope: f.scope, OwnerRunID: f.run.ID, AgentID: f.run.AssignedAgentID,
		Lease: AgentRunLeaseGuard{WorkerID: governedWorker, Now: f.clock}, Now: f.clock,
	}); err != nil {
		t.Fatal(err)
	}
	f.run = f.currentRun()
	// The release Skill is pinned, so the step calls it without a load.
	return dispatcher.ForClaim(LoopToolClaim{CapabilityConfig: modelstep.CapabilitySetConfig{PinnedSkillIDs: []string{"release"}}}), sessions
}

func TestWriteAfterHeldApprovalWaitsForDecision(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		var deployed []string
		f.dispatch = func(input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			environment, _ := input.Arguments["environment"].(string)
			deployed = append(deployed, environment)
			return map[string]interface{}{"providerResult": "delivered"}, nil
		}
		tools, sessions := governedToolExecutor(t, f)
		f.binding.Tools = tools
		step := appendToolStep(t, f.ctx, struct {
			AgentSessionStore
			AgentTurnStore
		}{sessions, f.store}, f.run, f.binding, governedWorker, governedStep, AgentTurnStatusCompleted, f.clock,
			ModelToolCall{ID: "call-prod", Name: governedDeploy, Arguments: deployArguments("production")},
			ModelToolCall{ID: "call-staging", Name: governedDeploy, Arguments: deployArguments("staging")},
			// A read after the skipped write must not overtake it.
			ModelToolCall{ID: "call-check", Name: governedStatus, Arguments: json.RawMessage(`{"environment":"check"}`)},
		)
		in := agentrun.StepToolsInput{Run: f.currentRun(), Step: step.turn, Binding: f.binding, WorkerID: governedWorker, Assistant: step.assistant, Entries: step.entries}
		out, err := tools.ExecuteStepTools(f.ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if out.Park == nil || out.Park.Status != AgentRunStatusWaitingForApproval || out.Park.Committed || out.SettlePending {
			t.Fatalf("outcome = %#v", out)
		}
		get := func(entry *SessionEntry) *SessionEntry {
			stored, err := sessions.GetSessionEntry(f.ctx, f.scope, entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			return stored
		}
		prod, staging := get(step.entries[0]), get(step.entries[1])
		if prod.State != ToolEntryAwaitingApproval || prod.ActionCallID == "" || prod.Tool.Wait == nil || prod.Tool.Wait.ApprovalID == "" {
			t.Fatalf("held write = %s %#v", prod.State, prod.Tool.Wait)
		}
		if staging.State != ToolEntryPending || staging.ActionCallID != "" || len(deployed) != 0 {
			t.Fatalf("later write = %s (call %q), deployed %v", staging.State, staging.ActionCallID, deployed)
		}
		if check := get(step.entries[2]); check.State != ToolEntryPending || check.ActionCallID != "" {
			t.Fatalf("read after the skipped write = %s (call %q)", check.State, check.ActionCallID)
		}
		calls, err := f.store.ListActionCalls(f.ctx, ActionFilter{Scope: f.scope, RunID: f.run.ID})
		if err != nil || len(calls) != 1 {
			t.Fatalf("action calls = %d, %v", len(calls), err)
		}
		// The loop parks at step end; the reviewer approves; a new claim resumes.
		f.parkOn(&GovernedPark{Status: out.Park.Status, Wake: out.Park.Wake, Summary: out.Park.Summary})
		delete(f.policy, "deploy")
		f.advance(time.Second)
		f.decide(prod.Tool.Wait.ApprovalID, ApprovalDecisionApprove, "")
		f.advance(time.Second)
		f.run = f.claim(governedWorker2)
		next, err := f.store.CreateAgentTurn(f.ctx, &AgentTurn{
			ID: "step-2", Scope: f.scope, RunID: f.run.ID, Status: AgentTurnStatusRunning, Producer: StepProducerModel, Revision: 1,
			CreatedAt: f.clock, UpdatedAt: f.clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		resumed, err := tools.ResumeOpenTools(f.ctx, agentrun.StepToolsInput{Run: f.currentRun(), Step: next, Binding: f.binding, WorkerID: governedWorker2})
		if err != nil {
			t.Fatal(err)
		}
		if resumed.Park != nil || resumed.Failure != nil {
			t.Fatalf("resume = %#v", resumed)
		}
		if strings.Join(deployed, ",") != "production,staging,check" {
			t.Fatalf("dispatched %v, want production, staging, then the read", deployed)
		}
		for _, entry := range step.entries {
			if stored := get(entry); stored.State != ToolEntryCompleted {
				t.Fatalf("after the decision %s = %s", entry.ToolCallID, stored.State)
			}
		}
	})
}

func TestReplanParkNilWhenHeldApprovalDecided(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newGovernedFixture(t, store)
		f.requireApprovalFor("deploy")
		tools, sessions := governedToolExecutor(t, f)
		f.binding.Tools = tools
		step := appendToolStep(t, f.ctx, struct {
			AgentSessionStore
			AgentTurnStore
		}{sessions, f.store}, f.run, f.binding, governedWorker, governedStep, AgentTurnStatusRunning, f.clock,
			ModelToolCall{ID: "call-prod", Name: governedDeploy, Arguments: deployArguments("production")},
		)
		out, err := tools.ExecuteStepTools(f.ctx, agentrun.StepToolsInput{
			Run: f.currentRun(), Step: step.turn, Binding: f.binding, WorkerID: governedWorker, Assistant: step.assistant, Entries: step.entries,
		})
		if err != nil || out.Park == nil {
			t.Fatalf("outcome = %#v, %v", out, err)
		}
		// Still pending: the re-plan parks on the same approval.
		park, err := tools.ReplanPark(f.ctx, agentrun.StepToolsInput{Run: f.currentRun(), Step: step.turn, Binding: f.binding, WorkerID: governedWorker})
		if err != nil || park == nil || park.Status != AgentRunStatusWaitingForApproval || park.Wake.Reference != out.Park.Wake.Reference {
			t.Fatalf("replan while pending = %#v, %v", park, err)
		}
		// Decided while the Run is held: the park CAS fails and the re-plan says do not park.
		entry, err := sessions.GetSessionEntry(f.ctx, f.scope, step.entries[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		f.advance(time.Second)
		f.decide(entry.Tool.Wait.ApprovalID, ApprovalDecisionApprove, "")
		park, err = tools.ReplanPark(f.ctx, agentrun.StepToolsInput{Run: f.currentRun(), Step: step.turn, Binding: f.binding, WorkerID: governedWorker})
		if err != nil || park != nil {
			t.Fatalf("replan after the decision = %#v, %v", park, err)
		}
	})
}

func TestRepeatedReadRefusedByExecutor(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{})
	var last toolStep
	for n := 1; n <= 3; n++ {
		last = f.step(fmt.Sprintf("step-%d", n), AgentTurnStatusCompleted, toolCall(fmt.Sprintf("call-%d", n), "workspace_read_file", `{"path":"same.md"}`))
		out, err := f.binding.Tools.ExecuteStepTools(f.ctx, f.input(last))
		if err != nil || out.Failure != nil {
			t.Fatalf("step %d = %#v, %v", n, out, err)
		}
	}
	third := f.entry(last.entries[0])
	if third.State != ToolEntryError || third.Tool.Guard == nil || third.Tool.Guard.Kind != action.RefusedLoopCallRepeat {
		t.Fatalf("third identical read = %s guard %#v", third.State, third.Tool.Guard)
	}
	if len(f.host.order) != 2 {
		t.Fatalf("host ran %d reads, want 2", len(f.host.order))
	}
}

// resumeNextStep runs ResumeOpenTools from a fresh step after step was
// appended (and finished and applied) but never went through phase 8.
func (f *toolExecFixture) resumeNextStep(step toolStep) agentrun.StepToolsOutcome {
	f.t.Helper()
	next, err := f.store.CreateAgentTurn(f.ctx, &AgentTurn{
		ID: "step-next", Scope: f.scope, RunID: f.run.ID, Status: AgentTurnStatusRunning, Producer: StepProducerModel, Revision: 1,
		CreatedAt: f.now, UpdatedAt: f.now,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	run := f.currentRun()
	run.LastAppliedTurn = step.turn.Sequence
	out, err := f.binding.Tools.ResumeOpenTools(f.ctx, agentrun.StepToolsInput{Run: run, Step: next, Binding: f.binding, WorkerID: toolExecWorker})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestResumeUncheckedStepRefusesSecondSettleCall(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{CanWait: true, CanAskUser: true})
	// The worker died between the append and phase 8.
	step := f.step("step-1", AgentTurnStatusCompleted,
		toolCall("call-wait", "wait", `{"goal":"a"}`),
		toolCall("call-ask", "ask_user", `{"goal":"b"}`),
	)
	out := f.resumeNextStep(step)
	if out.Failure != nil || out.Park != nil {
		t.Fatalf("outcome = %#v", out)
	}
	ask := f.entry(step.entries[1])
	if ask.State != ToolEntryError || resultValue(t, ask)["error"] != "control_one_settle_call" {
		t.Fatalf("second settle call = %s %v", ask.State, ask.Tool.Result)
	}
	if wait := f.entry(step.entries[0]); wait.State != ToolEntryCompleted || f.handlers[ControlToolWait].calls != 1 || f.handlers[ControlToolAskUser].calls != 0 {
		t.Fatalf("first settle call = %s, wait calls %d, ask calls %d", wait.State, f.handlers[ControlToolWait].calls, f.handlers[ControlToolAskUser].calls)
	}
}

func TestResumeUncheckedStepAppliesToolCheck(t *testing.T) {
	f := newToolExecFixture(t, agentrun.ControlAuthority{})
	checked := 0
	f.binding.ToolCheck = func(ctx context.Context, in agentrun.StepToolsInput, entry *SessionEntry) (string, error) {
		checked++
		return "Not in this conversation.", nil
	}
	step := f.step("step-1", AgentTurnStatusCompleted, toolCall("call-read", "workspace_read_file", `{"path":"a.md"}`))
	out := f.resumeNextStep(step)
	if out.Failure != nil || out.Park != nil || out.HostToolCalls != 0 {
		t.Fatalf("outcome = %#v", out)
	}
	read := f.entry(step.entries[0])
	if checked != 1 || read.State != ToolEntryError || read.Tool.Result.Status != ToolResultRefused || len(f.host.order) != 0 {
		t.Fatalf("checked %d, read = %s %#v, host ran %v", checked, read.State, read.Tool.Result, f.host.order)
	}
}

// The keeper looks the Run up by scope and id; a missing scope made every
// long computer or connected-app output fail the step.
func TestToolOutputInvocationCarriesTheRunsScopeAndAgent(t *testing.T) {
	run := &AgentRun{ID: "run-1", Scope: Scope{Kind: "tenant", ID: "9"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}, AssignedAgentID: "finn"}
	got := toolOutputInvocation(run, "computer_run_command", "openseal.computer", "run_command", "call-1")
	if got.RunID != "run-1" || got.Scope.Kind != "tenant" || got.Scope.ID != "9" || got.DeploymentID != "finn" || got.ExecutionDeploymentID != "finn" || got.ActionCallID != "call-1" {
		t.Fatalf("invocation = %+v", got)
	}
}
