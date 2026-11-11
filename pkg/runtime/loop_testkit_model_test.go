package runtime

// Run-loop test kit, part 1 (run-loop section 12 §3.1-3.4): the scripted
// model, its step view and result builders, fake host tools, the step host
// that joins them, the store matrix, the fake clock and the crash store.
// The loop fixture that drives the real RunLoop lives in
// loop_testkit_fixture_test.go.
//
// Fakes see only what the model sees: ModelStepRequest (its Entries, Tools
// and Purpose) and HostToolRequest. They never read run or turn state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Scripted model (§3.1)

// kitDefaultContextWindow is the context window every scripted result
// reports unless the scenario sets another, so compaction thresholds are
// deterministic.
const kitDefaultContextWindow int64 = 200_000

const (
	kitModelProvider = ModelStepProviderAnthropic
	kitModelName     = "fake-model"
)

// scriptedModel answers model steps from an ordered rule list. The first rule
// whose when matches and whose times is not used up replies. A step no rule
// matches is a test error, never a hang.
type scriptedModel struct {
	t             testing.TB
	mu            sync.Mutex
	rules         []*modelRule
	requests      []ModelStepRequest // every request, in order, deep-copied
	contextWindow int64
}

type modelRule struct {
	name  string
	when  func(v stepView) bool            // nil = always
	reply func(v stepView) ModelStepResult // nil when err is set
	err   error                            // e.g. &TurnHostFailure{Code: TurnHostContextExceeded}
	times int                              // 0 = unlimited; >0 = the rule is used at most this many times
	used  int
	// before runs inside ExecuteModelStep before replying. Scenarios use it to
	// post a steering message, advance the clock, or block until released.
	before func(ctx context.Context, v stepView) error
}

func newScriptedModel(t testing.TB, rules ...*modelRule) *scriptedModel {
	return &scriptedModel{t: t, rules: rules, contextWindow: kitDefaultContextWindow}
}

func (m *scriptedModel) add(rules ...*modelRule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = append(m.rules, rules...)
}

// setContextWindow changes the window reported on every later result.
func (m *scriptedModel) setContextWindow(tokens int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contextWindow = tokens
}

func (m *scriptedModel) ExecuteModelStep(ctx context.Context, req ModelStepRequest) (ModelStepResult, error) {
	copied := kitDeepCopyRequest(m.t, req)
	v := stepView{req: copied}
	m.mu.Lock()
	m.requests = append(m.requests, copied)
	var rule *modelRule
	for _, candidate := range m.rules {
		if candidate.times > 0 && candidate.used >= candidate.times {
			continue
		}
		if candidate.when != nil && !candidate.when(v) {
			continue
		}
		rule = candidate
		rule.used++
		break
	}
	window := m.contextWindow
	m.mu.Unlock()

	if rule == nil {
		m.t.Errorf("no scripted rule for step %s: %s", req.StepID, v.describe())
		return ModelStepResult{}, errors.New("scripted model: no rule")
	}
	if rule.before != nil {
		if err := rule.before(ctx, v); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ModelStepResult{}, ctxErr
			}
			return ModelStepResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ModelStepResult{}, err
	}
	if rule.err != nil {
		return ModelStepResult{}, rule.err
	}
	if rule.reply == nil {
		m.t.Errorf("scripted rule %q has neither reply nor err", rule.name)
		return ModelStepResult{}, fmt.Errorf("scripted model: rule %q has no reply", rule.name)
	}
	return finishScriptedResult(rule.reply(v), req, v.Step(), window), nil
}

// finishScriptedResult fills what a builder cannot know: the step identity,
// tool call ids "c<step>-<i>" (1-based i) and the model info.
func finishScriptedResult(res ModelStepResult, req ModelStepRequest, step int, window int64) ModelStepResult {
	res.APIVersion = ModelStepAPIVersion
	res.StepID = req.StepID
	if res.Assistant.Provider == "" {
		res.Assistant.Provider = kitModelProvider
	}
	if res.Assistant.Model == "" {
		res.Assistant.Model = kitModelName
	}
	if res.Assistant.StopReason == "" {
		res.Assistant.StopReason = ModelStopEnd
		if len(res.Assistant.ToolCalls) > 0 {
			res.Assistant.StopReason = ModelStopToolCalls
		}
	}
	calls := make([]ModelToolCall, len(res.Assistant.ToolCalls))
	copy(calls, res.Assistant.ToolCalls)
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("c%d-%d", step, i+1)
		}
	}
	res.Assistant.ToolCalls = calls
	if res.Model.Provider == "" {
		res.Model.Provider = res.Assistant.Provider
	}
	if res.Model.Model == "" {
		res.Model.Model = res.Assistant.Model
	}
	if res.Model.ContextTokens == 0 {
		res.Model.ContextTokens = window
	}
	return res
}

// calls returns the requests of one purpose ("" = all), in order.
func (m *scriptedModel) calls(purpose ModelStepPurpose) []ModelStepRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ModelStepRequest
	for _, req := range m.requests {
		if purpose == "" || req.Purpose == purpose {
			out = append(out, req)
		}
	}
	return out
}

func kitDeepCopyRequest(t testing.TB, req ModelStepRequest) ModelStepRequest {
	raw, err := json.Marshal(req)
	if err != nil {
		t.Errorf("scripted model: copy request: %v", err)
		return req
	}
	var out ModelStepRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Errorf("scripted model: copy request: %v", err)
		return req
	}
	return out
}

// ---------------------------------------------------------------------------
// Step view

// stepView is what a rule decides from: the rendered entries, the offered
// tools and the purpose.
type stepView struct{ req ModelStepRequest }

func (v stepView) Request() ModelStepRequest { return v.req }

func (v stepView) Purpose() ModelStepPurpose { return v.req.Purpose }

// Step is the 1-based step number: assistant entries in the rendered path
// plus one. Entries before a compaction are not rendered, so after a
// compaction the count restarts from the kept tail.
func (v stepView) Step() int {
	n := 1
	for _, e := range v.req.Entries {
		if e.Type == SessionEntryAssistant {
			n++
		}
	}
	return n
}

func (v stepView) LastEntry() *SessionEntryView {
	if len(v.req.Entries) == 0 {
		return nil
	}
	return &v.req.Entries[len(v.req.Entries)-1]
}

// LastUserText is the text of the newest user entry.
func (v stepView) LastUserText() string {
	for i := len(v.req.Entries) - 1; i >= 0; i-- {
		if e := v.req.Entries[i]; e.Type == SessionEntryUser && e.User != nil {
			return kitPartsText(e.User.Content)
		}
	}
	return ""
}

// UserTexts is every rendered user entry, in order. The kernel renders only
// the path since the latest compaction.
func (v stepView) UserTexts() []string {
	var out []string
	for _, e := range v.req.Entries {
		if e.Type == SessionEntryUser && e.User != nil {
			out = append(out, kitPartsText(e.User.Content))
		}
	}
	return out
}

// UserAuthor returns the attribution of the newest user entry whose text
// contains text, as "<kind>:<id>" ("person:ana", "agent:b"), or "requester"
// when the entry carries no author.
func (v stepView) UserAuthor(text string) (string, bool) {
	for i := len(v.req.Entries) - 1; i >= 0; i-- {
		e := v.req.Entries[i]
		if e.Type != SessionEntryUser || e.User == nil || !strings.Contains(kitPartsText(e.User.Content), text) {
			continue
		}
		if e.User.Author == nil {
			return "requester", true
		}
		id := e.User.Author.ID
		if id == "" {
			id = e.User.Author.Label
		}
		return e.User.Author.Kind + ":" + id, true
	}
	return "", false
}

// ToolResult is the newest tool entry for name: its result text and error flag.
func (v stepView) ToolResult(name string) (content string, isError bool, ok bool) {
	for i := len(v.req.Entries) - 1; i >= 0; i-- {
		e := v.req.Entries[i]
		if e.Type == SessionEntryTool && e.Tool != nil && e.Tool.Name == name {
			return kitPartsText(e.Tool.Result.Content), e.Tool.Result.IsError, true
		}
	}
	return "", false, false
}

// ToolResultsSinceUser is every tool entry after the newest user entry.
func (v stepView) ToolResultsSinceUser() []SessionEntryView {
	var out []SessionEntryView
	for i := len(v.req.Entries) - 1; i >= 0; i-- {
		e := v.req.Entries[i]
		if e.Type == SessionEntryUser {
			break
		}
		if e.Type == SessionEntryTool {
			out = append(out, e)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (v stepView) Notes() []string {
	var out []string
	for _, e := range v.req.Entries {
		if e.Type == SessionEntryNote && e.Note != nil {
			out = append(out, e.Note.Text)
		}
	}
	return out
}

// Compaction is the summary of the first rendered entry when it is a compaction.
func (v stepView) Compaction() (summary string, ok bool) {
	if len(v.req.Entries) == 0 {
		return "", false
	}
	first := v.req.Entries[0]
	if first.Type != SessionEntryCompaction || first.Compaction == nil {
		return "", false
	}
	return first.Compaction.Summary, true
}

// HasTool reports whether name is offered in the request's tools.
func (v stepView) HasTool(name string) bool {
	for _, tool := range v.req.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// describe is one line for failure messages: purpose, step, entry shape,
// last user text and offered tools.
func (v stepView) describe() string {
	shape := make([]string, 0, len(v.req.Entries))
	for _, e := range v.req.Entries {
		switch {
		case e.Type == SessionEntryTool && e.Tool != nil:
			shape = append(shape, "tool:"+e.Tool.Name+":"+string(e.Tool.State))
		case e.Type == SessionEntryNote && e.Note != nil:
			shape = append(shape, "note:"+e.Note.Kind)
		default:
			shape = append(shape, string(e.Type))
		}
	}
	tools := make([]string, 0, len(v.req.Tools))
	for _, tool := range v.req.Tools {
		tools = append(tools, tool.Name)
	}
	last := v.LastUserText()
	if len(last) > 80 {
		last = last[:80] + "..."
	}
	return fmt.Sprintf("purpose=%s step=%d attempt=%d entries=[%s] lastUser=%q tools=[%s]",
		v.req.Purpose, v.Step(), v.req.Attempt, strings.Join(shape, " "), last, strings.Join(tools, " "))
}

// kitPartsText joins the text of text parts and the compact JSON of json parts.
func kitPartsText(parts []ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		var s string
		switch p.Type {
		case "text":
			s = p.Text
		case "json":
			s = string(p.Value)
		default:
			continue
		}
		if b.Len() > 0 && s != "" {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Result builders

type tc struct {
	name string
	args map[string]any
}

func call(name string, args map[string]any) tc { return tc{name: name, args: args} }

// say is a final plain-text answer (StopReason end).
func say(text string) ModelStepResult {
	return ModelStepResult{Assistant: AssistantEntry{Text: text, StopReason: ModelStopEnd}}
}

// act narrates and calls tools. Call ids are filled by the scripted model as
// "c<step>-<i>" (1-based i).
func act(narration string, calls ...tc) ModelStepResult {
	toolCalls := make([]ModelToolCall, 0, len(calls))
	for _, c := range calls {
		args := c.args
		if args == nil {
			args = map[string]any{}
		}
		raw, err := json.Marshal(args)
		if err != nil {
			panic(fmt.Sprintf("act: arguments of %s: %v", c.name, err))
		}
		toolCalls = append(toolCalls, ModelToolCall{Name: c.name, Arguments: raw})
	}
	res := ModelStepResult{Assistant: AssistantEntry{Text: narration, ToolCalls: toolCalls, StopReason: ModelStopToolCalls}}
	if len(toolCalls) > 0 {
		res.Narration = narration
	} else {
		res.Assistant.StopReason = ModelStopEnd
	}
	return res
}

// finalAnswer is one call to turn_final_answer.
func finalAnswer(args map[string]any) ModelStepResult {
	return act("", call(string(ControlToolFinalAnswer), args))
}

func withUsage(r ModelStepResult, in, out int) ModelStepResult {
	r.Assistant.Usage.InputTokens = int64(in)
	r.Assistant.Usage.OutputTokens = int64(out)
	return r
}

// withReasoning adds opaque native reasoning blocks (Anthropic thinking
// blocks with a fake signature).
func withReasoning(r ModelStepResult, opaque ...string) ModelStepResult {
	native := &AssistantNative{}
	if r.Assistant.Native != nil {
		copied := *r.Assistant.Native
		native = &copied
		native.Anthropic = append([]json.RawMessage(nil), native.Anthropic...)
	}
	for _, data := range opaque {
		raw, _ := json.Marshal(map[string]string{"type": "thinking", "thinking": data, "signature": "fake"})
		native.Anthropic = append(native.Anthropic, raw)
	}
	r.Assistant.Native = native
	return r
}

// ---------------------------------------------------------------------------
// Fake host tools (§3.2)

type hostToolHandler func(ctx context.Context, req HostToolRequest) (HostToolResult, error)

// fakeHostTools answers host tools by HostToolRequest.Operation. An unknown
// operation is a test error and returns IsError.
type fakeHostTools struct {
	t                     testing.TB
	mu                    sync.Mutex
	handlers              map[string]hostToolHandler
	calls                 []HostToolRequest
	inFlight, maxInFlight int // concurrency probe for the parallel-read scenario
}

func newFakeHostTools(t testing.TB) *fakeHostTools {
	return &fakeHostTools{t: t, handlers: map[string]hostToolHandler{}}
}

func (h *fakeHostTools) handle(operation string, handler hostToolHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[operation] = handler
}

func (h *fakeHostTools) ExecuteHostTool(ctx context.Context, req HostToolRequest) (HostToolResult, error) {
	h.mu.Lock()
	h.calls = append(h.calls, req)
	handler := h.handlers[req.Operation]
	h.inFlight++
	if h.inFlight > h.maxInFlight {
		h.maxInFlight = h.inFlight
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.inFlight--
		h.mu.Unlock()
	}()

	if handler == nil {
		h.t.Errorf("fake host tools: unexpected host tool %q (entry %s)", req.Operation, req.ToolEntryID)
		return HostToolResult{
			APIVersion:   HostToolAPIVersion,
			InvocationID: req.InvocationID,
			Content:      []ContentPart{{Type: "text", Text: "unknown host tool " + req.Operation}},
			IsError:      true,
		}, nil
	}
	res, err := handler(ctx, req)
	if err != nil {
		return HostToolResult{}, err
	}
	if res.APIVersion == "" {
		res.APIVersion = HostToolAPIVersion
	}
	if res.InvocationID == "" {
		res.InvocationID = req.InvocationID
	}
	return res, nil
}

// requests returns the host tool requests for operation ("" = all), in order.
func (h *fakeHostTools) requests(operation string) []HostToolRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []HostToolRequest
	for _, req := range h.calls {
		if operation == "" || req.Operation == operation {
			out = append(out, req)
		}
	}
	return out
}

func (h *fakeHostTools) peakInFlight() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxInFlight
}

// hostText is a successful host tool result with one text part.
func hostText(text string) HostToolResult {
	return HostToolResult{Content: []ContentPart{{Type: "text", Text: text}}}
}

// fakeStepHost is the StepHost every kit scenario drives.
type fakeStepHost struct {
	*scriptedModel
	*fakeHostTools
}

var _ StepHost = fakeStepHost{}

// ---------------------------------------------------------------------------
// Store matrix (§3.3)

type kernelStoreCase struct {
	name string
	open func(*testing.T) KernelStore
}

// kernelStoreMatrix is SQLite always, plus Postgres when
// OPENSEAL_TEST_POSTGRES_DSN is set (schema prefix+"_"+uuid[:8], dropped in
// Cleanup). SQLite files go under t.TempDir, which follows TMPDIR (tmpfs).
func kernelStoreMatrix(t *testing.T, prefix string) []kernelStoreCase {
	t.Helper()
	cases := []kernelStoreCase{
		{name: "sqlite", open: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), prefix+".db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		cases = append(cases, kernelStoreCase{name: "postgres", open: func(t *testing.T) KernelStore {
			store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema(prefix+"_"+uuid.NewString()[:8]))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
				_ = store.Close()
			})
			return store
		}})
	}
	return cases
}

// eachStore runs test once per store in the matrix, as a subtest per store.
func eachStore(t *testing.T, prefix string, test func(t *testing.T, store KernelStore)) {
	t.Helper()
	for _, c := range kernelStoreMatrix(t, prefix) {
		c := c
		t.Run(c.name, func(t *testing.T) { test(t, c.open(t)) })
	}
}

// ---------------------------------------------------------------------------
// Fake clock

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start.UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---------------------------------------------------------------------------
// Crash simulation (§3.4)

var errSimulatedCrash = errors.New("simulated crash: replica is dead")

// crashStore wraps a KernelStore. After arm(), every write method the loop
// uses returns errSimulatedCrash and every read still works. It stands for a
// dead replica: no cleanup, no lease release. The session-store methods go
// to the wrapped store's AgentSessionStore.
type crashStore struct {
	KernelStore
	sessions AgentSessionStore
	dead     atomic.Bool
}

func newCrashStore(t testing.TB, store KernelStore) *crashStore {
	t.Helper()
	sessions, ok := store.(AgentSessionStore)
	if !ok {
		t.Fatalf("crash store: %T is not an AgentSessionStore", store)
	}
	return &crashStore{KernelStore: store, sessions: sessions}
}

func (s *crashStore) arm()    { s.dead.Store(true) }
func (s *crashStore) disarm() { s.dead.Store(false) }

func (s *crashStore) refused() error {
	if s.dead.Load() {
		return errSimulatedCrash
	}
	return nil
}

// Every overridden method is listed here, so a signature change in a store
// interface breaks the kit's build instead of silently bypassing the crash.
var _ = []any{
	(*crashStore).OpenConversationAgentSession,
	(*crashStore).CreateRunAgentSession,
	(*crashStore).AppendAgentSessionEntries,
	(*crashStore).TransitionToolEntry,
	(*crashStore).GetAgentSession,
	(*crashStore).GetSessionEntry,
	(*crashStore).LoadAgentSessionPath,
	(*crashStore).ListAgentSessionEntries,
	(*crashStore).CreateAgentRun,
	(*crashStore).CreateAgentRunWithEvent,
	(*crashStore).UpdateAgentRunWithEvent,
	(*crashStore).AppendActivity,
	(*crashStore).ClaimNextAgentRun,
	(*crashStore).RenewAgentRunLease,
	(*crashStore).CreateAgentTurn,
	(*crashStore).UpdateAgentTurn,
	(*crashStore).ClaimAgentTurn,
	(*crashStore).RecordAgentTurnStepModel,
	(*crashStore).CreateActionProposal,
	(*crashStore).ResolveApproval,
	(*crashStore).ClaimNextAction,
	(*crashStore).RenewActionLease,
	(*crashStore).PersistActionExecution,
}

var _ KernelStore = (*crashStore)(nil)
var _ AgentSessionStore = (*crashStore)(nil)

// Session store.

func (s *crashStore) OpenConversationAgentSession(ctx context.Context, req OpenConversationAgentSessionRequest) (*OpenAgentSessionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.OpenConversationAgentSession(ctx, req)
}

func (s *crashStore) CreateRunAgentSession(ctx context.Context, req CreateRunAgentSessionRequest) (*OpenAgentSessionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.CreateRunAgentSession(ctx, req)
}

func (s *crashStore) AppendAgentSessionEntries(ctx context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.AppendAgentSessionEntries(ctx, req)
}

func (s *crashStore) TransitionToolEntry(ctx context.Context, req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.sessions.TransitionToolEntry(ctx, req)
}

func (s *crashStore) GetAgentSession(ctx context.Context, scope Scope, sessionID string) (*AgentSession, error) {
	return s.sessions.GetAgentSession(ctx, scope, sessionID)
}

func (s *crashStore) GetSessionEntry(ctx context.Context, scope Scope, entryID string) (*SessionEntry, error) {
	return s.sessions.GetSessionEntry(ctx, scope, entryID)
}

func (s *crashStore) LoadAgentSessionPath(ctx context.Context, scope Scope, sessionID string) (*AgentSessionPath, error) {
	return s.sessions.LoadAgentSessionPath(ctx, scope, sessionID)
}

func (s *crashStore) ListAgentSessionEntries(ctx context.Context, filter SessionEntryFilter) ([]*SessionEntry, error) {
	return s.sessions.ListAgentSessionEntries(ctx, filter)
}

// Runs and activity.

func (s *crashStore) CreateAgentRun(ctx context.Context, run *AgentRun) error {
	if err := s.refused(); err != nil {
		return err
	}
	return s.KernelStore.CreateAgentRun(ctx, run)
}

func (s *crashStore) CreateAgentRunWithEvent(ctx context.Context, run *AgentRun, event *ActivityEvent) (*ActivityEvent, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateAgentRunWithEvent(ctx, run, event)
}

func (s *crashStore) UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.UpdateAgentRunWithEvent(ctx, run, expectedRevision, event, lease)
}

func (s *crashStore) AppendActivity(ctx context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.AppendActivity(ctx, event)
}

func (s *crashStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimNextAgentRun(ctx, claim)
}

func (s *crashStore) RenewAgentRunLease(ctx context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.RenewAgentRunLease(ctx, scope, runID, workerID, now, leaseDuration)
}

// Steps.

func (s *crashStore) CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateAgentTurn(ctx, turn)
}

func (s *crashStore) UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error {
	if err := s.refused(); err != nil {
		return err
	}
	return s.KernelStore.UpdateAgentTurn(ctx, turn, expectedRevision, workerID)
}

func (s *crashStore) ClaimAgentTurn(ctx context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimAgentTurn(ctx, scope, turnID, workerID, now, leaseDuration)
}

func (s *crashStore) RecordAgentTurnStepModel(ctx context.Context, turn *AgentTurn, expectedRevision int64, events []*ActivityEvent) error {
	if err := s.refused(); err != nil {
		return err
	}
	return s.KernelStore.RecordAgentTurnStepModel(ctx, turn, expectedRevision, events)
}

// Actions and approvals.

func (s *crashStore) CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateActionProposal(ctx, proposal)
}

func (s *crashStore) ResolveApproval(ctx context.Context, resolution ApprovalResolutionRecord) (*ApprovalResolutionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ResolveApproval(ctx, resolution)
}

func (s *crashStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimNextAction(ctx, claim)
}

func (s *crashStore) RenewActionLease(ctx context.Context, scope Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*ActionCall, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.RenewActionLease(ctx, scope, actionID, workerID, now, leaseDuration)
}

func (s *crashStore) PersistActionExecution(ctx context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error) {
	if err := s.refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.PersistActionExecution(ctx, execution)
}
