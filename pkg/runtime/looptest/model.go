package looptest

// Run-loop test kit, part 1 (run-loop section 12 §3.1-3.4): the scripted
// model, its step view and result builders, fake host tools, the step host
// that joins them, the store matrix, the fake clock and the crash store.
// The loop fixture that drives the real runloop.Loop lives in
// fixture.go.
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

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// ---------------------------------------------------------------------------
// Scripted model (§3.1)

// DefaultContextWindow is the context window every scripted result
// reports unless the scenario sets another, so compaction thresholds are
// deterministic.
const DefaultContextWindow int64 = 200_000

const (
	ModelProvider = models.ModelStepProviderAnthropic
	ModelName     = "fake-model"
)

// ScriptedModel answers model steps from an ordered rule list. The first rule
// whose when matches and whose times is not used up replies. A step no rule
// matches is a test error, never a hang.
type ScriptedModel struct {
	t             testing.TB
	Mu            sync.Mutex
	Rules         []*ModelRule
	requests      []models.ModelStepRequest // every request, in order, deep-copied
	contextWindow int64
}

type ModelRule struct {
	Name  string
	When  func(v StepView) bool                   // nil = always
	Reply func(v StepView) models.ModelStepResult // nil when err is set
	Err   error                                   // e.g. &TurnHostFailure{Code: TurnHostContextExceeded}
	Times int                                     // 0 = unlimited; >0 = the rule is used at most this many times
	Used  int
	// before runs inside ExecuteModelStep before replying. Scenarios use it to
	// post a steering message, advance the clock, or block until released.
	Before func(ctx context.Context, v StepView) error
}

func NewScriptedModel(t testing.TB, rules ...*ModelRule) *ScriptedModel {
	return &ScriptedModel{t: t, Rules: rules, contextWindow: DefaultContextWindow}
}

func (m *ScriptedModel) Add(rules ...*ModelRule) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.Rules = append(m.Rules, rules...)
}

// SetContextWindow changes the window reported on every later result.
func (m *ScriptedModel) SetContextWindow(tokens int64) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.contextWindow = tokens
}

func (m *ScriptedModel) ExecuteModelStep(ctx context.Context, req models.ModelStepRequest) (models.ModelStepResult, error) {
	copied := kitDeepCopyRequest(m.t, req)
	v := StepView{Req: copied}
	m.Mu.Lock()
	m.requests = append(m.requests, copied)
	var rule *ModelRule
	for _, candidate := range m.Rules {
		if candidate.Times > 0 && candidate.Used >= candidate.Times {
			continue
		}
		if candidate.When != nil && !candidate.When(v) {
			continue
		}
		rule = candidate
		rule.Used++
		break
	}
	window := m.contextWindow
	m.Mu.Unlock()

	if rule == nil {
		m.t.Errorf("no scripted rule for step %s: %s", req.StepID, v.Describe())
		return models.ModelStepResult{}, errors.New("scripted model: no rule")
	}
	if rule.Before != nil {
		if err := rule.Before(ctx, v); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return models.ModelStepResult{}, ctxErr
			}
			return models.ModelStepResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return models.ModelStepResult{}, err
	}
	if rule.Err != nil {
		return models.ModelStepResult{}, rule.Err
	}
	if rule.Reply == nil {
		m.t.Errorf("scripted rule %q has neither reply nor err", rule.Name)
		return models.ModelStepResult{}, fmt.Errorf("scripted model: rule %q has no reply", rule.Name)
	}
	return finishScriptedResult(rule.Reply(v), req, v.Step(), window), nil
}

// finishScriptedResult fills what a builder cannot know: the step identity,
// tool call ids "c<step>-<i>" (1-based i) and the model info.
func finishScriptedResult(res models.ModelStepResult, req models.ModelStepRequest, step int, window int64) models.ModelStepResult {
	res.APIVersion = models.ModelStepAPIVersion
	res.StepID = req.StepID
	if res.Assistant.Provider == "" {
		res.Assistant.Provider = ModelProvider
	}
	if res.Assistant.Model == "" {
		res.Assistant.Model = ModelName
	}
	if res.Assistant.StopReason == "" {
		res.Assistant.StopReason = models.ModelStopEnd
		if len(res.Assistant.ToolCalls) > 0 {
			res.Assistant.StopReason = models.ModelStopToolCalls
		}
	}
	calls := make([]models.ModelToolCall, len(res.Assistant.ToolCalls))
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

// Calls returns the requests of one purpose ("" = all), in order.
func (m *ScriptedModel) Calls(purpose models.ModelStepPurpose) []models.ModelStepRequest {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	var out []models.ModelStepRequest
	for _, req := range m.requests {
		if purpose == "" || req.Purpose == purpose {
			out = append(out, req)
		}
	}
	return out
}

func kitDeepCopyRequest(t testing.TB, req models.ModelStepRequest) models.ModelStepRequest {
	raw, err := json.Marshal(req)
	if err != nil {
		t.Errorf("scripted model: copy request: %v", err)
		return req
	}
	var out models.ModelStepRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Errorf("scripted model: copy request: %v", err)
		return req
	}
	return out
}

// ---------------------------------------------------------------------------
// Step view

// StepView is what a rule decides from: the rendered entries, the offered
// tools and the purpose.
type StepView struct{ Req models.ModelStepRequest }

func (v StepView) Request() models.ModelStepRequest { return v.Req }

func (v StepView) Purpose() models.ModelStepPurpose { return v.Req.Purpose }

// Step is the 1-based step number: assistant entries in the rendered path
// plus one. Entries before a compaction are not rendered, so after a
// compaction the count restarts from the kept tail.
func (v StepView) Step() int {
	n := 1
	for _, e := range v.Req.Entries {
		if e.Type == models.SessionEntryAssistant {
			n++
		}
	}
	return n
}

func (v StepView) LastEntry() *models.SessionEntryView {
	if len(v.Req.Entries) == 0 {
		return nil
	}
	return &v.Req.Entries[len(v.Req.Entries)-1]
}

// LastUserText is the text of the newest user entry.
func (v StepView) LastUserText() string {
	for i := len(v.Req.Entries) - 1; i >= 0; i-- {
		if e := v.Req.Entries[i]; e.Type == models.SessionEntryUser && e.User != nil {
			return PartsText(e.User.Content)
		}
	}
	return ""
}

// UserTexts is every rendered user entry, in order. The kernel renders only
// the path since the latest compaction.
func (v StepView) UserTexts() []string {
	var out []string
	for _, e := range v.Req.Entries {
		if e.Type == models.SessionEntryUser && e.User != nil {
			out = append(out, PartsText(e.User.Content))
		}
	}
	return out
}

// UserAuthor returns the attribution of the newest user entry whose text
// contains text, as "<kind>:<id>" ("person:ana", "agent:b"), or "requester"
// when the entry carries no author.
func (v StepView) UserAuthor(text string) (string, bool) {
	for i := len(v.Req.Entries) - 1; i >= 0; i-- {
		e := v.Req.Entries[i]
		if e.Type != models.SessionEntryUser || e.User == nil || !strings.Contains(PartsText(e.User.Content), text) {
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
func (v StepView) ToolResult(name string) (content string, isError bool, ok bool) {
	for i := len(v.Req.Entries) - 1; i >= 0; i-- {
		e := v.Req.Entries[i]
		if e.Type == models.SessionEntryTool && e.Tool != nil && e.Tool.Name == name {
			return PartsText(e.Tool.Result.Content), e.Tool.Result.IsError, true
		}
	}
	return "", false, false
}

// ToolResultsSinceUser is every tool entry after the newest user entry.
func (v StepView) ToolResultsSinceUser() []models.SessionEntryView {
	var out []models.SessionEntryView
	for i := len(v.Req.Entries) - 1; i >= 0; i-- {
		e := v.Req.Entries[i]
		if e.Type == models.SessionEntryUser {
			break
		}
		if e.Type == models.SessionEntryTool {
			out = append(out, e)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (v StepView) Notes() []string {
	var out []string
	for _, e := range v.Req.Entries {
		if e.Type == models.SessionEntryNote && e.Note != nil {
			out = append(out, e.Note.Text)
		}
	}
	return out
}

// Compaction is the summary of the first rendered entry when it is a compaction.
func (v StepView) Compaction() (summary string, ok bool) {
	if len(v.Req.Entries) == 0 {
		return "", false
	}
	first := v.Req.Entries[0]
	if first.Type != models.SessionEntryCompaction || first.Compaction == nil {
		return "", false
	}
	return first.Compaction.Summary, true
}

// HasTool reports whether name is offered in the request's tools.
func (v StepView) HasTool(name string) bool {
	for _, tool := range v.Req.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// Describe is one line for failure messages: purpose, step, entry shape,
// last user text and offered tools.
func (v StepView) Describe() string {
	shape := make([]string, 0, len(v.Req.Entries))
	for _, e := range v.Req.Entries {
		switch {
		case e.Type == models.SessionEntryTool && e.Tool != nil:
			shape = append(shape, "tool:"+e.Tool.Name+":"+string(e.Tool.State))
		case e.Type == models.SessionEntryNote && e.Note != nil:
			shape = append(shape, "note:"+e.Note.Kind)
		default:
			shape = append(shape, string(e.Type))
		}
	}
	tools := make([]string, 0, len(v.Req.Tools))
	for _, tool := range v.Req.Tools {
		tools = append(tools, tool.Name)
	}
	last := v.LastUserText()
	if len(last) > 80 {
		last = last[:80] + "..."
	}
	return fmt.Sprintf("purpose=%s step=%d attempt=%d entries=[%s] lastUser=%q tools=[%s]",
		v.Req.Purpose, v.Step(), v.Req.Attempt, strings.Join(shape, " "), last, strings.Join(tools, " "))
}

// PartsText joins the text of text parts and the compact JSON of json parts.
func PartsText(parts []models.ContentPart) string {
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

type ToolCall struct {
	name string
	args map[string]any
}

func Call(name string, args map[string]any) ToolCall { return ToolCall{name: name, args: args} }

// Say is a final plain-text answer (StopReason end).
func Say(text string) models.ModelStepResult {
	return models.ModelStepResult{Assistant: models.AssistantEntry{Text: text, StopReason: models.ModelStopEnd}}
}

// Act narrates and calls tools. Call ids are filled by the scripted model as
// "c<step>-<i>" (1-based i).
func Act(narration string, calls ...ToolCall) models.ModelStepResult {
	toolCalls := make([]models.ModelToolCall, 0, len(calls))
	for _, c := range calls {
		args := c.args
		if args == nil {
			args = map[string]any{}
		}
		raw, err := json.Marshal(args)
		if err != nil {
			panic(fmt.Sprintf("act: arguments of %s: %v", c.name, err))
		}
		toolCalls = append(toolCalls, models.ModelToolCall{Name: c.name, Arguments: raw})
	}
	res := models.ModelStepResult{Assistant: models.AssistantEntry{Text: narration, ToolCalls: toolCalls, StopReason: models.ModelStopToolCalls}}
	if len(toolCalls) > 0 {
		res.Narration = narration
	} else {
		res.Assistant.StopReason = models.ModelStopEnd
	}
	return res
}

// FinalAnswer is one call to turn_final_answer.
func FinalAnswer(args map[string]any) models.ModelStepResult {
	return Act("", Call(string(models.ControlToolFinalAnswer), args))
}

func WithUsage(r models.ModelStepResult, in, out int) models.ModelStepResult {
	r.Assistant.Usage.InputTokens = int64(in)
	r.Assistant.Usage.OutputTokens = int64(out)
	return r
}

// WithReasoning adds opaque native reasoning blocks (Anthropic thinking
// blocks with a fake signature).
func WithReasoning(r models.ModelStepResult, opaque ...string) models.ModelStepResult {
	native := &models.AssistantNative{}
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

type hostToolHandler func(ctx context.Context, req models.HostToolRequest) (models.HostToolResult, error)

// FakeHostTools answers host tools by HostToolRequest.Operation. An unknown
// operation is a test error and returns IsError.
type FakeHostTools struct {
	t                     testing.TB
	mu                    sync.Mutex
	handlers              map[string]hostToolHandler
	calls                 []models.HostToolRequest
	inFlight, maxInFlight int // concurrency probe for the parallel-read scenario
}

func NewFakeHostTools(t testing.TB) *FakeHostTools {
	return &FakeHostTools{t: t, handlers: map[string]hostToolHandler{}}
}

func (h *FakeHostTools) Handle(operation string, handler hostToolHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[operation] = handler
}

func (h *FakeHostTools) ExecuteHostTool(ctx context.Context, req models.HostToolRequest) (models.HostToolResult, error) {
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
		return models.HostToolResult{
			APIVersion:   models.HostToolAPIVersion,
			InvocationID: req.InvocationID,
			Content:      []models.ContentPart{{Type: "text", Text: "unknown host tool " + req.Operation}},
			IsError:      true,
		}, nil
	}
	res, err := handler(ctx, req)
	if err != nil {
		return models.HostToolResult{}, err
	}
	if res.APIVersion == "" {
		res.APIVersion = models.HostToolAPIVersion
	}
	if res.InvocationID == "" {
		res.InvocationID = req.InvocationID
	}
	return res, nil
}

// Requests returns the host tool requests for operation ("" = all), in order.
func (h *FakeHostTools) Requests(operation string) []models.HostToolRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []models.HostToolRequest
	for _, req := range h.calls {
		if operation == "" || req.Operation == operation {
			out = append(out, req)
		}
	}
	return out
}

func (h *FakeHostTools) PeakInFlight() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxInFlight
}

// HostText is a successful host tool result with one text part.
func HostText(text string) models.HostToolResult {
	return models.HostToolResult{Content: []models.ContentPart{{Type: "text", Text: text}}}
}

// FakeStepHost is the StepHost every kit scenario drives.
type FakeStepHost struct {
	*ScriptedModel
	*FakeHostTools
}

var _ agentrun.StepHost = FakeStepHost{}

// ---------------------------------------------------------------------------
// Store matrix (§3.3)

type StoreCase struct {
	Name string
	Open func(*testing.T) models.KernelStore
}

// StoreMatrix is SQLite always, plus Postgres when
// OPENSEAL_TEST_POSTGRES_DSN is set (schema prefix+"_"+uuid[:8], dropped in
// Cleanup). SQLite files go under t.TempDir, which follows TMPDIR (tmpfs).
func StoreMatrix(t *testing.T, prefix string) []StoreCase {
	t.Helper()
	cases := []StoreCase{
		{Name: "sqlite", Open: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), prefix+".db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		cases = append(cases, StoreCase{Name: "postgres", Open: func(t *testing.T) models.KernelStore {
			schema := storetest.NewPostgresSchema(t, prefix+"_")
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			store, err := postgres.New(ctx, schema.DSN, postgres.WithSchema(schema.Name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}})
	}
	return cases
}

// EachStore runs test once per store in the matrix, as a subtest per store.
func EachStore(t *testing.T, prefix string, test func(t *testing.T, store models.KernelStore)) {
	t.Helper()
	for _, c := range StoreMatrix(t, prefix) {
		c := c
		t.Run(c.Name, func(t *testing.T) { test(t, c.Open(t)) })
	}
}

// ---------------------------------------------------------------------------
// Fake clock

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start.UTC()} }

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

var ErrSimulatedCrash = errors.New("simulated crash: replica is dead")

// CrashStore wraps a KernelStore. After arm(), every write method the loop
// uses returns ErrSimulatedCrash and every read still works. It stands for a
// dead replica: no cleanup, no lease release. The session-store methods go
// to the wrapped store's AgentSessionStore.
type CrashStore struct {
	models.KernelStore
	sessions models.AgentSessionStore
	dead     atomic.Bool
}

func NewCrashStore(t testing.TB, store models.KernelStore) *CrashStore {
	t.Helper()
	sessions, ok := store.(models.AgentSessionStore)
	if !ok {
		t.Fatalf("crash store: %T is not an AgentSessionStore", store)
	}
	return &CrashStore{KernelStore: store, sessions: sessions}
}

func (s *CrashStore) Arm()    { s.dead.Store(true) }
func (s *CrashStore) Disarm() { s.dead.Store(false) }

func (s *CrashStore) Refused() error {
	if s.dead.Load() {
		return ErrSimulatedCrash
	}
	return nil
}

// Every overridden method is listed here, so a signature change in a store
// interface breaks the kit's build instead of silently bypassing the crash.
var _ = []any{
	(*CrashStore).OpenConversationAgentSession,
	(*CrashStore).CreateRunAgentSession,
	(*CrashStore).AppendAgentSessionEntries,
	(*CrashStore).TransitionToolEntry,
	(*CrashStore).GetAgentSession,
	(*CrashStore).GetSessionEntry,
	(*CrashStore).LoadAgentSessionPath,
	(*CrashStore).ListAgentSessionEntries,
	(*CrashStore).CreateAgentRun,
	(*CrashStore).CreateAgentRunWithEvent,
	(*CrashStore).UpdateAgentRunWithEvent,
	(*CrashStore).AppendActivity,
	(*CrashStore).ClaimNextAgentRun,
	(*CrashStore).RenewAgentRunLease,
	(*CrashStore).CreateAgentTurn,
	(*CrashStore).UpdateAgentTurn,
	(*CrashStore).ClaimAgentTurn,
	(*CrashStore).RecordAgentTurnStepModel,
	(*CrashStore).CreateActionProposal,
	(*CrashStore).ResolveApproval,
	(*CrashStore).ClaimNextAction,
	(*CrashStore).RenewActionLease,
	(*CrashStore).PersistActionExecution,
}

var _ models.KernelStore = (*CrashStore)(nil)
var _ models.AgentSessionStore = (*CrashStore)(nil)

// Session store.

func (s *CrashStore) OpenConversationAgentSession(ctx context.Context, req models.OpenConversationAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.sessions.OpenConversationAgentSession(ctx, req)
}

func (s *CrashStore) CreateRunAgentSession(ctx context.Context, req models.CreateRunAgentSessionRequest) (*models.OpenAgentSessionResult, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.sessions.CreateRunAgentSession(ctx, req)
}

func (s *CrashStore) AppendAgentSessionEntries(ctx context.Context, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.sessions.AppendAgentSessionEntries(ctx, req)
}

func (s *CrashStore) TransitionToolEntry(ctx context.Context, req models.ToolEntryTransitionRequest) (*models.ToolEntryTransitionResult, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.sessions.TransitionToolEntry(ctx, req)
}

func (s *CrashStore) GetAgentSession(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSession, error) {
	return s.sessions.GetAgentSession(ctx, scope, sessionID)
}

func (s *CrashStore) GetSessionEntry(ctx context.Context, scope models.Scope, entryID string) (*models.SessionEntry, error) {
	return s.sessions.GetSessionEntry(ctx, scope, entryID)
}

func (s *CrashStore) LoadAgentSessionPath(ctx context.Context, scope models.Scope, sessionID string) (*models.AgentSessionPath, error) {
	return s.sessions.LoadAgentSessionPath(ctx, scope, sessionID)
}

func (s *CrashStore) ListAgentSessionEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
	return s.sessions.ListAgentSessionEntries(ctx, filter)
}

// Runs and activity.

func (s *CrashStore) CreateAgentRun(ctx context.Context, run *models.AgentRun) error {
	if err := s.Refused(); err != nil {
		return err
	}
	return s.KernelStore.CreateAgentRun(ctx, run)
}

func (s *CrashStore) CreateAgentRunWithEvent(ctx context.Context, run *models.AgentRun, event *models.ActivityEvent) (*models.ActivityEvent, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateAgentRunWithEvent(ctx, run, event)
}

func (s *CrashStore) UpdateAgentRunWithEvent(ctx context.Context, run *models.AgentRun, expectedRevision int64, event *models.ActivityEvent, lease *models.AgentRunLeaseGuard) (*models.ActivityEvent, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.UpdateAgentRunWithEvent(ctx, run, expectedRevision, event, lease)
}

func (s *CrashStore) AppendActivity(ctx context.Context, event *models.ActivityEvent) (*models.ActivityEvent, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.AppendActivity(ctx, event)
}

func (s *CrashStore) ClaimNextAgentRun(ctx context.Context, claim models.AgentRunClaim) (*models.AgentRun, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimNextAgentRun(ctx, claim)
}

func (s *CrashStore) RenewAgentRunLease(ctx context.Context, scope models.Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*models.AgentRun, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.RenewAgentRunLease(ctx, scope, runID, workerID, now, leaseDuration)
}

// Steps.

func (s *CrashStore) CreateAgentTurn(ctx context.Context, turn *models.AgentTurn) (*models.AgentTurn, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateAgentTurn(ctx, turn)
}

func (s *CrashStore) UpdateAgentTurn(ctx context.Context, turn *models.AgentTurn, expectedRevision int64, workerID string) error {
	if err := s.Refused(); err != nil {
		return err
	}
	return s.KernelStore.UpdateAgentTurn(ctx, turn, expectedRevision, workerID)
}

func (s *CrashStore) ClaimAgentTurn(ctx context.Context, scope models.Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*models.AgentTurn, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimAgentTurn(ctx, scope, turnID, workerID, now, leaseDuration)
}

func (s *CrashStore) RecordAgentTurnStepModel(ctx context.Context, turn *models.AgentTurn, expectedRevision int64, events []*models.ActivityEvent) error {
	if err := s.Refused(); err != nil {
		return err
	}
	return s.KernelStore.RecordAgentTurnStepModel(ctx, turn, expectedRevision, events)
}

// Actions and approvals.

func (s *CrashStore) CreateActionProposal(ctx context.Context, proposal models.ActionProposalRecord) (*models.ActionProposalResult, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.CreateActionProposal(ctx, proposal)
}

func (s *CrashStore) ResolveApproval(ctx context.Context, resolution models.ApprovalResolutionRecord) (*models.ApprovalResolutionResult, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ResolveApproval(ctx, resolution)
}

func (s *CrashStore) ClaimNextAction(ctx context.Context, claim models.ActionClaim) (*models.ActionCall, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.ClaimNextAction(ctx, claim)
}

func (s *CrashStore) RenewActionLease(ctx context.Context, scope models.Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*models.ActionCall, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.RenewActionLease(ctx, scope, actionID, workerID, now, leaseDuration)
}

func (s *CrashStore) PersistActionExecution(ctx context.Context, execution models.ActionExecutionRecord) (*models.ActionExecutionResult, error) {
	if err := s.Refused(); err != nil {
		return nil, err
	}
	return s.KernelStore.PersistActionExecution(ctx, execution)
}
