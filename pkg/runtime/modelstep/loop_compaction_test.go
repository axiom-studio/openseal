package modelstep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Fakes

// fakeSummaryHost answers summary calls from a script. Every request is
// validated as the real host would, so a malformed request fails the test.
type fakeSummaryHost struct {
	t        *testing.T
	requests []models.ModelStepRequest
	script   []func(req models.ModelStepRequest) (models.ModelStepResult, error)
}

func (h *fakeSummaryHost) ExecuteModelStep(_ context.Context, req models.ModelStepRequest) (models.ModelStepResult, error) {
	h.t.Helper()
	if err := req.Validate(); err != nil {
		h.t.Fatalf("summary request %d is invalid: %v", len(h.requests), err)
	}
	h.requests = append(h.requests, req)
	if len(h.requests) > len(h.script) {
		h.t.Fatalf("unexpected summary call %d", len(h.requests))
	}
	return h.script[len(h.requests)-1](req)
}

func (h *fakeSummaryHost) ExecuteHostTool(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	h.t.Helper()
	h.t.Fatalf("the compactor ran a host tool")
	return models.HostToolResult{}, nil
}

func summaryAnswer(text string, input, output int64) func(models.ModelStepRequest) (models.ModelStepResult, error) {
	return func(req models.ModelStepRequest) (models.ModelStepResult, error) {
		return models.ModelStepResult{
			APIVersion: models.ModelStepAPIVersion, StepID: req.StepID,
			Assistant: models.AssistantEntry{
				Provider: models.ModelStepProviderAnthropic, Model: "claude-x", Text: text, StopReason: models.ModelStopEnd,
				Usage: models.ModelStepUsage{InputTokens: input, OutputTokens: output, Cost: 0.01, Attempts: 1},
			},
			Model: models.ModelStepModelInfo{Provider: models.ModelStepProviderAnthropic, Model: "claude-x", ContextTokens: 64000},
		}, nil
	}
}

func summaryHostFailure(code string) func(models.ModelStepRequest) (models.ModelStepResult, error) {
	return func(models.ModelStepRequest) (models.ModelStepResult, error) {
		return models.ModelStepResult{}, &TurnHostFailure{Code: code, Message: "host said " + code}
	}
}

// fakeStepRequests renders the path the way the step would send it.
type fakeStepRequests struct{}

func (fakeStepRequests) PrepareModelStep(_ context.Context, in agentrun.StepInput) (models.ModelStepRequest, error) {
	var views []models.SessionEntryView
	if in.Path.Compaction != nil {
		views = append(views, testEntryView(in.Path.Compaction))
	}
	for _, entry := range in.Path.Entries {
		views = append(views, testEntryView(entry))
	}
	return models.ModelStepRequest{
		APIVersion: models.ModelStepAPIVersion, Purpose: models.ModelStepPurposeStep, Scope: in.Run.Scope,
		RunID: in.Run.ID, StepID: in.Step.ID, Attempt: in.Attempt, SessionID: in.Path.Session.ID,
		AgentID: in.Run.AssignedAgentID, DefinitionID: "def", DefinitionVersion: "1", Model: "claude-x",
		System:  models.ModelStepSystem{AgentInstructions: []string{"Be useful."}, Sections: []models.ModelStepSystemSection{{ID: "rules", Title: "Rules", Text: "Follow them."}}},
		Tools:   []models.ModelStepTool{{Name: "turn_final_answer", Description: "answer", InputSchema: map[string]interface{}{"type": "object"}, Group: models.ModelStepToolHarness}},
		Entries: views, MaxOutputTokens: 4000,
		Stream: models.ModelStepStream{AnswerPreview: true, FinalAnswerTool: "turn_final_answer"},
	}, nil
}

func testEntryView(entry *models.SessionEntry) models.SessionEntryView {
	view := models.SessionEntryView{ID: entry.ID, Sequence: entry.Sequence, Type: entry.Type, CreatedAt: entry.CreatedAt}
	switch entry.Type {
	case models.SessionEntryUser:
		view.User = &models.UserEntryView{Content: entry.User.Content}
	case models.SessionEntryAssistant:
		assistant := *entry.Assistant
		assistant.Limits = nil
		view.Assistant = &assistant
	case models.SessionEntryTool:
		view.Tool = &models.ToolEntryView{
			CallID: entry.ToolCallID, Name: entry.Tool.Name, Arguments: entry.Tool.Arguments, State: entry.State,
			Result: models.ToolResultView{Content: entry.Tool.Result.Content, IsError: entry.Tool.Result.IsError}, ActionCallID: entry.ActionCallID,
		}
	case models.SessionEntryNote:
		view.Note = &models.NoteEntryView{Kind: string(entry.Note.Kind), Text: entry.Note.Text}
	case models.SessionEntryCompaction:
		view.Compaction = &models.CompactionEntryView{
			Summary: CompactionSummaryText(entry.Compaction), FirstKeptEntryID: entry.Compaction.FirstKeptEntryID,
			TokensBefore: entry.Compaction.TokensBefore,
		}
	}
	return view
}

// ---------------------------------------------------------------------------
// Fixture

type compactionFixture struct {
	*storetest.SessionFixture
	store     *sqlite.Store
	runID     string
	sessionID string
	host      *fakeSummaryHost
	compactor *LoopCompactor
	step      int
}

func newCompactionFixture(t *testing.T) *compactionFixture {
	t.Helper()
	store := newModelStepTestStore(t)
	f := &storetest.SessionFixture{
		T: t, Ctx: context.Background(), Store: store,
		Scope: models.Scope{Kind: "tenant", ID: "compact-" + uuid.NewString()[:8]},
		Now:   time.Now().UTC().Truncate(time.Millisecond),
	}
	run, session := f.ConversationRun("agent-a")
	host := &fakeSummaryHost{t: t}
	fx := &compactionFixture{SessionFixture: f, store: store, runID: run.ID, sessionID: session.ID, host: host}
	ids := 0
	fx.compactor = &LoopCompactor{
		Host: host, Sessions: store, Requests: fakeStepRequests{},
		Now:   func() time.Time { return f.Now },
		NewID: func() string { ids++; return fmt.Sprintf("cmp-%d", ids) },
	}
	return fx
}

func (fx *compactionFixture) user(id string, tokens int64) *models.SessionEntry {
	return storetest.WithTokens(storetest.UserSessionEntry(id, "message "+id), tokens)
}

func (fx *compactionFixture) note(id string, tokens int64) *models.SessionEntry {
	return storetest.WithTokens(storetest.NoteSessionEntry(id, models.NoteRunContext), tokens)
}

// stepWithTool is one model step that called one tool, recorded with the
// given action call id.
func (fx *compactionFixture) stepWithTool(id, actionCallID string, assistantTokens, toolTokens int64) []*models.SessionEntry {
	stepID := "step-" + id
	assistant := storetest.WithTokens(&models.SessionEntry{ID: "a-" + id, StepID: stepID, Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
		Provider: models.ModelStepProviderAnthropic, Model: "claude-x", Text: "checking", StopReason: models.ModelStopToolCalls,
		ToolCalls: []models.ModelToolCall{{ID: "call-" + id, Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)}},
	}}, assistantTokens)
	tool := storetest.WithTokens(&models.SessionEntry{ID: "t-" + id, StepID: stepID, Type: models.SessionEntryTool, State: models.ToolEntryCompleted,
		ToolCallID: "call-" + id, ActionCallID: actionCallID, Tool: &models.ToolEntry{
			Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`), SideEffect: models.ToolSideEffectRead,
			Result: &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{storetest.TextPart("contents " + id)}},
		}}, toolTokens)
	return []*models.SessionEntry{assistant, tool}
}

// finalStep is a model step without tool calls that carries the window and
// its real prompt size (the estimate's anchor).
func (fx *compactionFixture) finalStep(id string, tokens, promptTokens int64, limits bool) *models.SessionEntry {
	entry := storetest.WithTokens(&models.SessionEntry{ID: "a-" + id, StepID: "step-" + id, Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
		Provider: models.ModelStepProviderAnthropic, Model: "claude-x", Text: "done " + id, StopReason: models.ModelStopEnd,
	}}, tokens)
	if limits {
		entry.Assistant.Limits = &models.AssistantModelLimits{ContextTokens: 64000, PromptTokens: promptTokens}
	}
	return entry
}

func (fx *compactionFixture) add(entries ...*models.SessionEntry) {
	fx.T.Helper()
	fx.MustAppend(fx.sessionID, fx.runID, entries...)
}

// history appends the standard path (64k window, anchor 45000 + 5000 after):
// u0 a-s1 t-s1(ac_1) u2 | a-s2 t-s2(ac_2) a-s3 u5. A threshold cut keeps
// a-s2 onwards.
func (fx *compactionFixture) history(limits bool) {
	fx.add(fx.user("u0", 10000))
	fx.add(fx.stepWithTool("s1", "ac_1", 2000, 10000)...)
	fx.add(fx.user("u2", 3000))
	fx.add(fx.stepWithTool("s2", "ac_2", 2000, 8000)...)
	fx.add(fx.finalStep("s3", 2000, 45000, limits))
	fx.add(fx.user("u5", 3000))
}

func (fx *compactionFixture) input() agentrun.StepInput {
	fx.T.Helper()
	fx.step++
	run := fx.Run(fx.runID)
	path := fx.Path(fx.sessionID)
	return agentrun.StepInput{
		Run: run, Step: &models.AgentTurn{ID: fmt.Sprintf("step-now-%d", fx.step), Scope: fx.Scope, RunID: fx.runID, Sequence: int64(10 + fx.step), Status: models.AgentTurnStatusRunning},
		Session: path.Session, Path: path, Binding: &agentrun.TurnRunnerBinding{Model: "claude-x"},
		Mode: agentrun.StepModeNormal, WorkerID: storetest.SessionWorker,
	}
}

func (fx *compactionFixture) compactions() []*models.SessionEntry {
	fx.T.Helper()
	entries, err := fx.store.ListAgentSessionEntries(fx.Ctx, models.SessionEntryFilter{Scope: fx.Scope, SessionID: fx.sessionID, Types: []models.SessionEntryType{models.SessionEntryCompaction}})
	if err != nil {
		fx.T.Fatalf("list compactions: %v", err)
	}
	return entries
}

func (fx *compactionFixture) compactedEvents() []*models.ActivityEvent {
	fx.T.Helper()
	events, err := fx.store.ListActivity(fx.Ctx, models.ActivityFilter{Scope: fx.Scope, RunID: fx.runID, EventTypes: []string{compactionEventType}, Limit: 100})
	if err != nil {
		fx.T.Fatalf("list activity: %v", err)
	}
	return events
}

func viewIDs(views []models.SessionEntryView) string {
	ids := make([]string, len(views))
	for i, view := range views {
		ids[i] = view.ID
	}
	return strings.Join(ids, ",")
}

func requireLoopFailure(t *testing.T, err error, kind, code string) {
	t.Helper()
	var failure *LoopFailureError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a LoopFailureError %s/%s", err, kind, code)
	}
	if failure.Failure.Kind != kind || failure.Failure.Code != code {
		t.Fatalf("failure = %+v, want %s/%s", failure.Failure, kind, code)
	}
}

// ---------------------------------------------------------------------------
// Threshold

func TestCompactThresholdSendsStepRequestAsSummary(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.host.script = append(fx.host.script, summaryAnswer("## Objective\nship it", 50500, 900))
	in := fx.input()
	if !fx.compactor.Due(in) {
		t.Fatal("Due = false, want true over the 48000-token threshold")
	}
	step, _ := fakeStepRequests{}.PrepareModelStep(fx.Ctx, in)

	usage, err := fx.compactor.CompactIfNeeded(fx.Ctx, in)
	if err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	if len(fx.host.requests) != 1 {
		t.Fatalf("summary calls = %d, want 1", len(fx.host.requests))
	}
	req := fx.host.requests[0]
	if req.Purpose != models.ModelStepPurposeSummary || req.SummaryInstruction != compactionSummaryInstruction || req.MaxOutputTokens != compactionSummaryMaxOutputTokens {
		t.Fatalf("summary request purpose=%s maxOutput=%d instruction set=%v", req.Purpose, req.MaxOutputTokens, req.SummaryInstruction != "")
	}
	if !reflect.DeepEqual(req.System, step.System) || !reflect.DeepEqual(req.Tools, step.Tools) {
		t.Fatal("summary System/Tools differ from the step's")
	}
	if got, want := viewIDs(req.Entries), viewIDs(step.Entries); got != want {
		t.Fatalf("summary entries = %s, want the full path %s", got, want)
	}
	if req.StepID != in.Step.ID || req.RunID != fx.runID || req.SessionID != fx.sessionID || req.Stream.AnswerPreview {
		t.Fatalf("summary identity/stream = %s/%s/%s preview=%v", req.StepID, req.RunID, req.SessionID, req.Stream.AnswerPreview)
	}
	if usage.InputTokens != 50500 || usage.OutputTokens != 900 {
		t.Fatalf("usage = %+v", usage)
	}

	path := fx.Path(fx.sessionID)
	if path.Compaction == nil {
		t.Fatal("no compaction on the path")
	}
	c := path.Compaction.Compaction
	if c.Summary != "## Objective\nship it" || c.FirstKeptEntryID != "a-s2" || c.Trigger != models.CompactionTriggerThreshold ||
		c.TokensBefore != 50000 || c.Provider != "anthropic" || c.Model != "claude-x" || c.Usage.InputTokens != 50500 {
		t.Fatalf("compaction = %+v", c)
	}
	if !reflect.DeepEqual(c.ActionCallIDs, []string{"ac_1"}) {
		t.Fatalf("actionCallIds = %v, want [ac_1]", c.ActionCallIDs)
	}
	if path.Compaction.StepID != in.Step.ID || path.Compaction.Step != in.Step.Sequence || path.Compaction.Tokens != EstimateEntryTokens(path.Compaction) {
		t.Fatalf("compaction envelope step=%s/%d tokens=%d", path.Compaction.StepID, path.Compaction.Step, path.Compaction.Tokens)
	}
	if got := storetest.EntryIDs(path.Entries); got != "a-s2,t-s2,a-s3,u5" {
		t.Fatalf("kept path = %s", got)
	}
	// One step has to run on the new compaction before another threshold one.
	if fx.compactor.Due(fx.input()) {
		t.Fatal("Due right after a compaction")
	}
}

func TestCompactThresholdSkipsWhenNothingToCompact(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.add(fx.user("u0", 1000))
	fx.add(fx.finalStep("s1", 1000, 60000, true))
	usage, err := fx.compactor.Compact(fx.Ctx, fx.input(), false)
	if err != nil || usage != (models.ModelStepUsage{}) || len(fx.host.requests) != 0 || len(fx.compactions()) != 0 {
		t.Fatalf("usage=%+v err=%v calls=%d", usage, err, len(fx.host.requests))
	}
}

func TestCompactThresholdSkipsWhenTheSummaryDoesNotFit(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.host.script = append(fx.host.script, summaryHostFailure(TurnHostContextExceeded))
	usage, err := fx.compactor.CompactIfNeeded(fx.Ctx, fx.input())
	if err != nil || usage != (models.ModelStepUsage{}) || len(fx.compactions()) != 0 {
		t.Fatalf("usage=%+v err=%v compactions=%d", usage, err, len(fx.compactions()))
	}
}

func TestCompactAppendsEntryAndEventUnderLease(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.host.script = append(fx.host.script, summaryAnswer("summary", 100, 10), summaryAnswer("summary", 100, 10))

	if _, err := fx.compactor.CompactIfNeeded(fx.Ctx, fx.input()); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	events := fx.compactedEvents()
	if len(events) != 1 {
		t.Fatalf("session.compacted events = %d", len(events))
	}
	compaction := fx.compactions()[0]
	event := events[0]
	payload := event.Payload
	wantAfter := compaction.Tokens + 8000 + 2000 + 2000 + 3000 + defaultPrefixTokens
	if event.Summary != "Summarized earlier context" || event.TurnID != compaction.StepID || event.Visibility != models.ActivityVisibilityScope ||
		payload["sessionId"] != fx.sessionID || payload["entryId"] != compaction.ID || payload["firstKeptEntryId"] != "a-s2" ||
		payload["trigger"] != models.CompactionTriggerThreshold || fmt.Sprint(payload["tokensBefore"]) != "50000" ||
		fmt.Sprint(payload["tokensAfter"]) != fmt.Sprint(wantAfter) {
		t.Fatalf("event = %+v payload = %v (want tokensAfter %d)", event, payload, wantAfter)
	}
	if _, ok := payload["summary"]; ok || strings.Contains(fmt.Sprint(payload), "summary\"") {
		t.Fatal("the summary text leaked into activity")
	}

	// A worker without the lease writes nothing.
	fx.add(fx.stepWithTool("s6", "ac_3", 1000, 30000)...)
	fx.add(fx.finalStep("s7", 1000, 60000, true))
	in := fx.input()
	in.WorkerID = "other-worker"
	_, err := fx.compactor.Compact(fx.Ctx, in, false)
	if !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("lost lease err = %v", err)
	}
	if len(fx.compactions()) != 1 || len(fx.compactedEvents()) != 1 {
		t.Fatalf("compactions=%d events=%d after a lost lease", len(fx.compactions()), len(fx.compactedEvents()))
	}
}

func TestCompactInvalidSummaryRetriesOnceThenFails(t *testing.T) {
	t.Run("invalid twice", func(t *testing.T) {
		fx := newCompactionFixture(t)
		fx.history(true)
		fx.host.script = append(fx.host.script, summaryAnswer("   ", 100, 1), summaryHostFailure(modelStepSummaryFailed))
		usage, err := fx.compactor.CompactIfNeeded(fx.Ctx, fx.input())
		requireLoopFailure(t, err, RunFailureKindContextOverflow, RunFailureCodeCompactionFailed)
		if len(fx.host.requests) != 2 || !reflect.DeepEqual(fx.host.requests[0], fx.host.requests[1]) {
			t.Fatalf("calls = %d (want 2 identical)", len(fx.host.requests))
		}
		if usage.InputTokens != 100 || len(fx.compactions()) != 0 {
			t.Fatalf("usage = %+v compactions=%d", usage, len(fx.compactions()))
		}
	})
	t.Run("oversized then valid", func(t *testing.T) {
		fx := newCompactionFixture(t)
		fx.history(true)
		fx.host.script = append(fx.host.script, summaryAnswer(strings.Repeat("x", models.MaxCompactionSummaryBytes+1), 100, 16384), summaryAnswer("good", 120, 20))
		usage, err := fx.compactor.CompactIfNeeded(fx.Ctx, fx.input())
		if err != nil {
			t.Fatalf("CompactIfNeeded: %v", err)
		}
		if usage.InputTokens != 220 || usage.OutputTokens != 16404 || usage.Attempts != 2 {
			t.Fatalf("usage = %+v, want both calls summed", usage)
		}
		if got := fx.compactions(); len(got) != 1 || got[0].Compaction.Summary != "good" {
			t.Fatalf("compactions = %v", got)
		}
	})
	t.Run("host error unchanged", func(t *testing.T) {
		fx := newCompactionFixture(t)
		fx.history(true)
		fx.host.script = append(fx.host.script, func(models.ModelStepRequest) (models.ModelStepResult, error) {
			return models.ModelStepResult{}, NewTurnHostFailure(StepHostFailureUnreachable, "down", true)
		})
		_, err := fx.compactor.CompactIfNeeded(fx.Ctx, fx.input())
		if !errors.Is(err, ErrTurnHostUnavailable) {
			t.Fatalf("err = %v, want the host error", err)
		}
	})
}

func TestCompactEntryActionCallIDsAreUnionOfPreviousAndSpan(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.host.script = append(fx.host.script, summaryAnswer("first", 100, 10), summaryAnswer("second", 100, 10))
	if _, err := fx.compactor.CompactIfNeeded(fx.Ctx, fx.input()); err != nil {
		t.Fatalf("first compaction: %v", err)
	}
	fx.add(fx.stepWithTool("s6", "ac_3", 1000, 20000)...)
	fx.add(fx.finalStep("s7", 1000, 46000, true))
	fx.add(fx.user("u8", 2000))
	in := fx.input()
	if _, err := fx.compactor.CompactIfNeeded(fx.Ctx, in); err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	// The second summary call reads the first compaction plus the whole path.
	if got := viewIDs(fx.host.requests[1].Entries); got != "cmp-1,a-s2,t-s2,a-s3,u5,a-s6,t-s6,a-s7,u8" {
		t.Fatalf("second summary entries = %s", got)
	}
	path := fx.Path(fx.sessionID)
	c := path.Compaction.Compaction
	if c.Summary != "second" || c.FirstKeptEntryID != "a-s6" || !reflect.DeepEqual(c.ActionCallIDs, []string{"ac_1", "ac_2"}) {
		t.Fatalf("second compaction = %+v", c)
	}
}

// ---------------------------------------------------------------------------
// Overflow

func TestCompactOverflowSendsOnlySummarizedSpan(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.host.script = append(fx.host.script, summaryAnswer("overflow summary", 30000, 500))
	ctx := WithCompactionPressure(fx.Ctx, &HostedContextPressure{EstimatedTokens: 70000, LimitTokens: 60000})
	in := fx.input()
	in.Attempt = 0
	if _, err := fx.compactor.Compact(ctx, in, true); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(fx.host.requests) != 1 {
		t.Fatalf("calls = %d", len(fx.host.requests))
	}
	req := fx.host.requests[0]
	if got := viewIDs(req.Entries); got != "u0,a-s1,t-s1,u2" {
		t.Fatalf("overflow entries = %s, want only the summarized span", got)
	}
	step, _ := fakeStepRequests{}.PrepareModelStep(fx.Ctx, in)
	if !reflect.DeepEqual(req.System, step.System) || !reflect.DeepEqual(req.Tools, step.Tools) || req.Purpose != models.ModelStepPurposeSummary {
		t.Fatal("overflow summary must keep the step's system and tools")
	}
	c := fx.Path(fx.sessionID).Compaction.Compaction
	if c.Trigger != models.CompactionTriggerOverflow || c.FirstKeptEntryID != "a-s2" || c.Summary != "overflow summary" {
		t.Fatalf("compaction = %+v", c)
	}
}

// foldHistory has no window on the path (the overflow sizes from the
// rejection's limit) and a summarized span that needs two chunks:
// [u0 n0] [a-s1 t-s1 u2], kept a-s2 onwards.
func (fx *compactionFixture) foldHistory() {
	fx.add(fx.user("u0", 15000), fx.note("n0", 15000))
	fx.add(fx.stepWithTool("s1", "ac_1", 1000, 25000)...)
	fx.add(fx.user("u2", 1000))
	fx.add(fx.stepWithTool("s2", "ac_2", 1000, 4000)...)
	fx.add(fx.finalStep("s3", 1000, 0, false))
	fx.add(fx.user("u5", 2000))
}

func foldPressure(ctx context.Context) context.Context {
	return WithCompactionPressure(ctx, &HostedContextPressure{EstimatedTokens: 100000, LimitTokens: 80000})
}

func TestCompactOverflowFoldsChunks(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.foldHistory()
	fx.host.script = append(fx.host.script, summaryAnswer("part one", 30000, 400), summaryAnswer("parts one and two", 40000, 600))
	usage, err := fx.compactor.Compact(foldPressure(fx.Ctx), fx.input(), true)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(fx.host.requests) != 2 {
		t.Fatalf("calls = %d, want 2", len(fx.host.requests))
	}
	if got := viewIDs(fx.host.requests[0].Entries); got != "u0,n0" {
		t.Fatalf("chunk 0 = %s", got)
	}
	second := fx.host.requests[1].Entries
	if got := viewIDs(second[1:]); got != "a-s1,t-s1,u2" || second[0].Type != models.SessionEntryCompaction {
		t.Fatalf("chunk 1 = %s", viewIDs(second))
	}
	if second[0].Compaction.Summary != CompactionSummaryText(&models.CompactionEntry{Summary: "part one"}) {
		t.Fatalf("fold view = %q", second[0].Compaction.Summary)
	}
	if usage.InputTokens != 70000 || usage.OutputTokens != 1000 {
		t.Fatalf("usage = %+v, want both calls", usage)
	}
	c := fx.Path(fx.sessionID).Compaction.Compaction
	if c.Summary != "parts one and two" || c.FirstKeptEntryID != "a-s2" || c.Usage.InputTokens != 70000 ||
		!reflect.DeepEqual(c.ActionCallIDs, []string{"ac_1"}) {
		t.Fatalf("compaction = %+v", c)
	}
}

func TestCompactOverflowSplitsARejectedChunkOnce(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.foldHistory()
	fx.host.script = append(fx.host.script,
		summaryAnswer("one", 1, 1), summaryHostFailure(TurnHostContextExceeded),
		summaryAnswer("two", 1, 1), summaryAnswer("three", 1, 1))
	if _, err := fx.compactor.Compact(foldPressure(fx.Ctx), fx.input(), true); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(fx.host.requests) != 4 {
		t.Fatalf("calls = %d", len(fx.host.requests))
	}
	if a, b := viewIDs(fx.host.requests[2].Entries[1:]), viewIDs(fx.host.requests[3].Entries[1:]); a != "a-s1,t-s1" || b != "u2" {
		t.Fatalf("halves = %s | %s", a, b)
	}
	if c := fx.Path(fx.sessionID).Compaction.Compaction; c.Summary != "three" {
		t.Fatalf("summary = %q", c.Summary)
	}

	// A half rejected again cannot be compacted.
	fx2 := newCompactionFixture(t)
	fx2.foldHistory()
	fx2.host.script = append(fx2.host.script,
		summaryAnswer("one", 1, 1), summaryHostFailure(TurnHostContextExceeded), summaryHostFailure(TurnHostContextExceeded))
	_, err := fx2.compactor.Compact(foldPressure(fx2.Ctx), fx2.input(), true)
	requireLoopFailure(t, err, RunFailureKindContextOverflow, TurnHostContextExceeded)
	if len(fx2.compactions()) != 0 {
		t.Fatal("a failed compaction was appended")
	}
}

func TestCompactOverflowFoldLimitFails(t *testing.T) {
	fx := newCompactionFixture(t)
	for i := 0; i < 10; i++ {
		fx.add(fx.user(fmt.Sprintf("u%d", i), 25000))
	}
	fx.add(fx.stepWithTool("s1", "ac_1", 1000, 4000)...)
	fx.add(fx.finalStep("s2", 1000, 0, false))
	_, err := fx.compactor.Compact(foldPressure(fx.Ctx), fx.input(), true)
	requireLoopFailure(t, err, RunFailureKindContextOverflow, TurnHostContextExceeded)
	if len(fx.host.requests) != 0 || len(fx.compactions()) != 0 {
		t.Fatalf("calls=%d compactions=%d", len(fx.host.requests), len(fx.compactions()))
	}
}

func TestCompactOverflowImpossibleForSingleStep(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.add(fx.user("u0", 50000))
	fx.add(fx.finalStep("s1", 1000, 0, false))
	_, err := fx.compactor.Compact(foldPressure(fx.Ctx), fx.input(), true)
	requireLoopFailure(t, err, RunFailureKindContextOverflow, TurnHostContextExceeded)
	failure := err.(*LoopFailureError).Failure
	if failure.Message != runFailureMessageContextExceeded || failure.Explainable {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestCompactOverflowSingleEntryChunkIsContextOverflow(t *testing.T) {
	fx := newCompactionFixture(t)
	// The overflow keep leaves only u0 to summarize, with no previous
	// compaction: one entry is too few for a summary request.
	fx.add(fx.user("u0", 40000))
	fx.add(fx.stepWithTool("s1", "ac_1", 1000, 20000)...)
	fx.add(fx.stepWithTool("s2", "ac_2", 1000, 1000)...)
	fx.add(fx.finalStep("s3", 1000, 0, false))
	_, err := fx.compactor.Compact(foldPressure(fx.Ctx), fx.input(), true)
	if err == nil {
		t.Fatalf("compacted a span whose first chunk is one entry")
	}
	requireLoopFailure(t, err, RunFailureKindContextOverflow, TurnHostContextExceeded)
	for i, req := range fx.host.requests {
		if len(req.Entries) < 2 {
			t.Fatalf("request %d had %d entries", i, len(req.Entries))
		}
	}
}

// ---------------------------------------------------------------------------
// Budget

func TestCompactChargesBudgetNotTurns(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.UpdateRun(fx.runID, func(run *models.AgentRun) {
		run.Budget = &models.BudgetPolicy{MaxTurns: 10, MaxInputTokens: 1_000_000, MaxOutputTokens: 1_000_000}
	})
	fx.compactor.Budget = NewRunActivityCompactionBudget(fx.store, activity.NewRunActivityService(fx.store, fx.store), func() time.Time { return fx.Now })
	fx.host.script = append(fx.host.script, summaryAnswer("summary", 50500, 900))
	in := fx.input()
	before := in.Run.Revision
	usage, err := fx.compactor.CompactIfNeeded(fx.Ctx, in)
	if err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	run := fx.Run(fx.runID)
	if run.BudgetUsage.InputTokens != 50500 || run.BudgetUsage.OutputTokens != 900 || run.BudgetUsage.Turns != 0 || run.BudgetUsage.CostMicros != 10000 {
		t.Fatalf("budget usage = %+v", run.BudgetUsage)
	}
	if _, open := run.BudgetReservations[compactionBudgetReservationID(in.Step.ID)]; open || len(run.BudgetReservations) != 0 {
		t.Fatalf("reservations = %v", run.BudgetReservations)
	}
	if in.Run.Revision != run.Revision || run.Revision != before+2 {
		t.Fatalf("in.Run revision %d, stored %d, before %d", in.Run.Revision, run.Revision, before)
	}
	if usage.InputTokens != 50500 {
		t.Fatalf("usage = %+v", usage)
	}
	steps, err := fx.store.ListAgentTurns(fx.Ctx, models.AgentTurnFilter{Scope: fx.Scope, RunID: fx.runID})
	if err != nil || len(steps) != 0 {
		t.Fatalf("compaction wrote %d agent_turns rows", len(steps))
	}
}

func TestCompactSettlesBudgetAfterAHeartbeatDuringTheSummary(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.UpdateRun(fx.runID, func(run *models.AgentRun) {
		run.Budget = &models.BudgetPolicy{MaxInputTokens: 1_000_000, MaxOutputTokens: 1_000_000}
	})
	fx.compactor.Budget = NewRunActivityCompactionBudget(fx.store, activity.NewRunActivityService(fx.store, fx.store), func() time.Time { return fx.Now })
	in := fx.input()
	// The Run moved on after the step read it: a heartbeat before the
	// reservation, and another while the summary call runs.
	if _, err := fx.store.RenewAgentRunLease(fx.Ctx, fx.Scope, fx.runID, storetest.SessionWorker, fx.Now, time.Hour); err != nil {
		t.Fatalf("renew: %v", err)
	}
	answer := summaryAnswer("summary", 50500, 900)
	fx.host.script = append(fx.host.script, func(req models.ModelStepRequest) (models.ModelStepResult, error) {
		if _, err := fx.store.RenewAgentRunLease(fx.Ctx, fx.Scope, fx.runID, storetest.SessionWorker, fx.Now, time.Hour); err != nil {
			t.Fatalf("renew: %v", err)
		}
		return answer(req)
	})
	if _, err := fx.compactor.CompactIfNeeded(fx.Ctx, in); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	run := fx.Run(fx.runID)
	if len(run.BudgetReservations) != 0 {
		t.Fatalf("reservations = %v", run.BudgetReservations)
	}
	if run.BudgetUsage.InputTokens != 50500 || run.BudgetUsage.OutputTokens != 900 {
		t.Fatalf("budget usage = %+v", run.BudgetUsage)
	}
	if in.Run.Revision != run.Revision || len(fx.compactions()) != 1 {
		t.Fatalf("in.Run revision %d, stored %d, compactions %d", in.Run.Revision, run.Revision, len(fx.compactions()))
	}
	events, err := fx.store.ListActivity(fx.Ctx, models.ActivityFilter{Scope: fx.Scope, RunID: fx.runID, EventTypes: []string{"budget.settled"}, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("settled events = %d, %v", len(events), err)
	}
	if events[0].UsageDelta != nil {
		t.Fatalf("settle wrote activity usage %+v; the step owns it", events[0].UsageDelta)
	}
}

func TestCompactBudgetExhaustedFailsWithoutCalling(t *testing.T) {
	fx := newCompactionFixture(t)
	fx.history(true)
	fx.UpdateRun(fx.runID, func(run *models.AgentRun) {
		run.Budget = &models.BudgetPolicy{MaxOutputTokens: 1000}
	})
	fx.compactor.Budget = NewRunActivityCompactionBudget(fx.store, activity.NewRunActivityService(fx.store, fx.store), func() time.Time { return fx.Now })
	_, err := fx.compactor.CompactIfNeeded(fx.Ctx, fx.input())
	requireLoopFailure(t, err, "budget", "budget_exhausted")
	if len(fx.host.requests) != 0 || len(fx.compactions()) != 0 {
		t.Fatalf("calls=%d compactions=%d", len(fx.host.requests), len(fx.compactions()))
	}
}

// ---------------------------------------------------------------------------
// Rendering

func TestCompactionSummaryTextFormat(t *testing.T) {
	got := CompactionSummaryText(&models.CompactionEntry{Summary: "  ## Objective\nShip.\n", ActionCallIDs: []string{"ac_01JA", "ac_01JB"}})
	want := "The conversation and work before this point were compacted. This summary replaces them; the messages after it are verbatim.\n\n" +
		"## Objective\nShip.\n\nAction calls recorded before this point: ac_01JA, ac_01JB"
	if got != want {
		t.Fatalf("text = %q", got)
	}
	view := compactionFoldView("s", 1, "S", []models.SessionEntryView{{ID: "x", Sequence: 4}, {ID: "y", Sequence: 9}})
	if view.Sequence != 10 || view.Compaction.FirstKeptEntryID != "x" || view.Compaction.Summary != CompactionSummaryText(&models.CompactionEntry{Summary: "S"}) {
		t.Fatalf("fold view = %+v", view)
	}
}

func TestCompactionModelPrefersStepThenBindingThenNewest(t *testing.T) {
	path := windowPath(nil,
		windowAssistant("a1", 1, "old", 10, 0),
		&models.SessionEntry{ID: "mc", Sequence: 2, Type: models.SessionEntryModelChange, ModelChange: &models.ModelChangeEntry{Provider: "anthropic", Model: "new"}},
		windowAssistant("a3", 3, "resolved", 10, 0))
	cases := []struct {
		name    string
		step    string
		binding string
		want    string
	}{
		{"step", "stepmodel", "old", "stepmodel"},
		{"binding on path", "", "old", "old"},
		{"newest after change", "", "auto", "resolved"},
	}
	for _, tc := range cases {
		in := agentrun.StepInput{Step: &models.AgentTurn{Model: tc.step}, Binding: &agentrun.TurnRunnerBinding{Model: tc.binding}, Path: path}
		if got := compactionModel(in); got != tc.want {
			t.Fatalf("%s: model = %q, want %q", tc.name, got, tc.want)
		}
	}
	stale := windowPath(nil, windowAssistant("a1", 1, "old", 10, 0),
		&models.SessionEntry{ID: "mc", Sequence: 2, Type: models.SessionEntryModelChange, ModelChange: &models.ModelChangeEntry{Provider: "anthropic", Model: "new"}})
	if got := compactionModel(agentrun.StepInput{Step: &models.AgentTurn{}, Binding: &agentrun.TurnRunnerBinding{Model: "auto"}, Path: stale}); got != "auto" {
		t.Fatalf("model after a change = %q, want the binding's", got)
	}
}
