package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type guardFixture struct {
	t     *testing.T
	run   *AgentRun
	path  *AgentSessionPath
	calls map[string]*ActionCall
	now   time.Time
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	scope := Scope{Kind: "workspace", ID: "ws-guards"}
	return &guardFixture{
		t:     t,
		run:   &AgentRun{ID: "run-guards", Scope: scope, Kind: RunKindConversation, SessionID: "session-guards"},
		path:  &AgentSessionPath{},
		calls: map[string]*ActionCall{},
		now:   time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
	}
}

// add records a terminal loop call and its tool entry on the path.
func (f *guardFixture) add(id, skillID, action string, status ActionCallStatus, output map[string]interface{}, errorCode string, details map[string]string) *SessionEntry {
	f.t.Helper()
	f.now = f.now.Add(time.Second)
	call := &ActionCall{
		ID: id, Scope: f.run.Scope, RunID: f.run.ID, SkillID: skillID, Action: action, Status: status,
		Origin: ActionOriginLoop, Output: output, ErrorCode: errorCode, ErrorDetails: details,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.calls[id] = call
	state := ToolEntryCompleted
	if status != ActionCallStatusSucceeded {
		state = ToolEntryError
	}
	entry := &SessionEntry{
		ID: "entry-" + id, Scope: f.run.Scope, SessionID: f.run.SessionID, RunID: f.run.ID,
		StepID: "step-" + id, Type: SessionEntryTool, State: state, ToolCallID: "tc-" + id, ActionCallID: id,
		Tool: &ToolEntry{Name: skillID + "." + action},
	}
	f.path.Entries = append(f.path.Entries, entry)
	return entry
}

func (f *guardFixture) views() []governedToolView {
	return governedToolViews(f.path, f.run.ID, f.calls)
}

func (f *guardFixture) rateLimitedLightpanda(id string) {
	f.add(id, lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusFailed, nil,
		"source_rate_limited", map[string]string{"httpStatus": "429"})
}

func TestLiveBrowserRefusedAfterRateLimitedLightpandaFailure(t *testing.T) {
	f := newGuardFixture(t)
	f.add("read-ok", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusSucceeded, map[string]interface{}{"title": "x"}, "", nil)
	f.rateLimitedLightpanda("read-429")
	// An unrelated Skill's success does not clear the Lightpanda verdict.
	f.add("other", "skill-weather", "forecast", ActionCallStatusSucceeded, nil, "", nil)

	views := f.views()
	if !browserSourceRateLimited(views) {
		t.Fatal("expected the typed 429 Lightpanda failure to rate-limit the live browser")
	}
	result, refused := refuseLiveBrowserWhileRateLimited(views, "live-browser-open")
	if !refused || result == nil {
		t.Fatal("expected the live browser call to be refused")
	}
	if result.Status != ToolResultRefused || !result.IsError || len(result.Content) != 1 || result.Content[0].Type != ContentPartJSON {
		t.Fatalf("unexpected refusal result: %+v", result)
	}
	var content map[string]string
	if err := json.Unmarshal(result.Content[0].Value, &content); err != nil {
		t.Fatal(err)
	}
	if content["error"] != liveBrowserRateLimitedError || content["status"] != "refused" || content["code"] != "source_rate_limited" {
		t.Fatalf("unexpected refusal content: %v", content)
	}
	// Only live-browser actions are refused; the tool list itself never changes.
	if _, refused := refuseLiveBrowserWhileRateLimited(views, "lightpanda-read"); refused {
		t.Fatal("a non live-browser action must not be refused")
	}

	// A batch whose every read failed with a 429 item counts too.
	g := newGuardFixture(t)
	g.add("batch", lightpandaBrowserSkillID, "lightpanda-read-many", ActionCallStatusFailed, nil, "source_reads_failed",
		map[string]string{"failedCount": "2", "totalCount": "2", "failures": `[{"index":0,"failureKind":"source_rate_limited","httpStatus":429}]`})
	if !browserSourceRateLimited(g.views()) {
		t.Fatal("expected a rate-limited batch item to rate-limit the live browser")
	}
}

func TestLiveBrowserAllowedAfterNewerLightpandaSuccess(t *testing.T) {
	f := newGuardFixture(t)
	f.rateLimitedLightpanda("read-429")
	f.add("read-ok", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusSucceeded, nil, "", nil)
	if browserSourceRateLimited(f.views()) {
		t.Fatal("a newer Lightpanda success must clear the rate limit")
	}
	if _, refused := refuseLiveBrowserWhileRateLimited(f.views(), "live-browser-open"); refused {
		t.Fatal("live browser must be allowed after a newer success")
	}
	// A newer typed non-429 source failure also decides (not rate-limited).
	g := newGuardFixture(t)
	g.rateLimitedLightpanda("read-429")
	g.add("read-500", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusFailed, nil, "source_http_error", map[string]string{"httpStatus": "500"})
	if browserSourceRateLimited(g.views()) {
		t.Fatal("a newer typed non-429 source failure supersedes the earlier rate limit")
	}
}

func TestUntypedSourceErrorDoesNotRateLimit(t *testing.T) {
	f := newGuardFixture(t)
	// Error text alone, or an unknown code, is never evidence of HTTP status.
	call := f.add("untyped", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusFailed, nil, "", nil)
	f.calls[call.ActionCallID].Error = "HTTP 429 Too Many Requests"
	f.add("unknown", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusFailed, nil, "source_made_up", map[string]string{"httpStatus": "429"})
	if browserSourceRateLimited(f.views()) {
		t.Fatal("untyped or unknown source errors must not rate-limit")
	}
	if _, refused := refuseLiveBrowserWhileRateLimited(f.views(), "live-browser-open"); refused {
		t.Fatal("live browser must not be refused on untyped evidence")
	}
	// An untyped newer failure does not clear an earlier typed rate limit.
	g := newGuardFixture(t)
	g.rateLimitedLightpanda("read-429")
	g.add("untyped", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusFailed, nil, "", nil)
	if !browserSourceRateLimited(g.views()) {
		t.Fatal("an untyped failure must not clear the typed rate limit")
	}
}

func TestFinalAnswerParksForOTPWhenRunCanAsk(t *testing.T) {
	f := newGuardFixture(t)
	f.add("otp", liveBrowserSkillID, "live-browser-submit", ActionCallStatusSucceeded, map[string]interface{}{"status": "otp_required"}, "", nil)
	plan := finalAnswerPark(f.run, f.views(), true, "  What is the code the bank just sent you? ", nil)
	if plan == nil {
		t.Fatal("expected the Run to park on the person's reply")
	}
	if !plan.Deliver || plan.Status != AgentRunStatusWaitingForEvent || plan.Wake == nil || plan.Wake.Type != "user_message" ||
		plan.Summary != "What is the code the bank just sent you?" {
		t.Fatalf("unexpected park: %+v wake=%+v", plan, plan.Wake)
	}
	if finalAnswerPark(f.run, f.views(), true, "   ", nil) != nil {
		t.Fatal("an empty answer completes normally")
	}
	// The code was submitted: the newest live-browser success no longer waits.
	f.add("submitted", liveBrowserSkillID, "live-browser-type", ActionCallStatusSucceeded, map[string]interface{}{"status": "ok"}, "", nil)
	if finalAnswerPark(f.run, f.views(), true, "Done.", nil) != nil {
		t.Fatal("a newer live-browser success clears the wait")
	}
}

func TestFinalAnswerCompletesForOTPWhenRunCannotAsk(t *testing.T) {
	f := newGuardFixture(t)
	f.add("otp", liveBrowserSkillID, "live-browser-submit", ActionCallStatusSucceeded, map[string]interface{}{"status": "otp_required"}, "", nil)
	if plan := finalAnswerPark(f.run, f.views(), false, "What is the code?", nil); plan != nil {
		t.Fatalf("a Run that cannot ask its conversation must complete, got %+v", plan)
	}
}

func TestFinalAnswerParksForApproveInApp(t *testing.T) {
	f := newGuardFixture(t)
	f.add("pay", liveBrowserSkillID, "live-browser-click", ActionCallStatusSucceeded, map[string]interface{}{"paymentChallenge": "approve_in_app"}, "", nil)
	plan := finalAnswerPark(f.run, f.views(), true, "Please approve the payment in your bank app, then tell me.", nil)
	if plan == nil || !plan.Deliver || plan.Wake == nil || plan.Wake.Type != "user_message" {
		t.Fatalf("expected an in-app approval to park on the reply, got %+v", plan)
	}
	// A failed live-browser call is not a wait.
	g := newGuardFixture(t)
	g.add("pay", liveBrowserSkillID, "live-browser-click", ActionCallStatusFailed, map[string]interface{}{"paymentChallenge": "approve_in_app"}, "", nil)
	if finalAnswerPark(g.run, g.views(), true, "answer", nil) != nil {
		t.Fatal("a failed call never parks")
	}
}

func setupTaskRun(f *guardFixture) {
	f.run.Kind = RunKindAgentWork
	f.run.Context = map[string]interface{}{ConversationTaskContextKey: "task-1"}
}

func (f *guardFixture) requestSetup(id, requestID string) *SessionEntry {
	return f.add(id, SkillManagementSkillID, SkillActionRequestSetup, ActionCallStatusSucceeded, map[string]interface{}{
		"setupRequest": map[string]interface{}{"id": requestID, "status": "pending", "runId": f.run.ID, "actionCallId": id},
	}, "", nil)
}

func TestBackgroundTaskParksOnPendingSetup(t *testing.T) {
	f := newGuardFixture(t)
	setupTaskRun(f)
	f.requestSetup("setup-call", "setup-1")
	plan := finalAnswerPark(f.run, f.views(), true, "I asked you to connect the account.", nil)
	if plan == nil {
		t.Fatal("expected the task Run to park on the pending setup")
	}
	if plan.Deliver || plan.Status != AgentRunStatusWaitingForEvent || plan.Wake == nil ||
		plan.Wake.Type != ConversationTaskSkillSetupWakeType || plan.Wake.Reference != "setup-1" || plan.Summary != skillSetupWaitSummary {
		t.Fatalf("unexpected setup park: %+v wake=%+v", plan, plan.Wake)
	}

	// list_setup_requests reports the same pending form.
	g := newGuardFixture(t)
	setupTaskRun(g)
	g.add("list", SkillManagementSkillID, SkillActionListSetupRequests, ActionCallStatusSucceeded, map[string]interface{}{
		"requests": []interface{}{
			map[string]interface{}{"id": "other-run", "status": "pending", "runId": "run-else", "actionCallId": "x"},
			map[string]interface{}{"id": "done", "status": "resolved", "runId": g.run.ID, "actionCallId": "y"},
			map[string]interface{}{"id": "setup-2", "status": "pending", "runId": g.run.ID, "actionCallId": "z"},
		},
	}, "", nil)
	if id := pendingSkillSetupID(g.run, g.views(), nil); id != "setup-2" {
		t.Fatalf("expected setup-2, got %q", id)
	}
	// A pending setup takes precedence over a browser wait on task Runs.
	g.add("otp", liveBrowserSkillID, "live-browser-submit", ActionCallStatusSucceeded, map[string]interface{}{"status": "otp_required"}, "", nil)
	if id := pendingSkillSetupID(g.run, g.views(), nil); id != "setup-2" {
		t.Fatalf("a later non-setup call must not clear the pending setup, got %q", id)
	}
	if plan := finalAnswerPark(g.run, g.views(), true, "code?", nil); plan == nil || plan.Deliver || plan.Wake.Reference != "setup-2" {
		t.Fatalf("expected the setup park, got %+v", plan)
	}
}

func TestSetupResolutionInterventionClearsPark(t *testing.T) {
	f := newGuardFixture(t)
	setupTaskRun(f)
	f.requestSetup("setup-call", "setup-1")
	receipt := func(mutate func(*SkillSetupResolutionReceipt)) []AgentRunIntervention {
		r := &SkillSetupResolutionReceipt{Scope: f.run.Scope, RunID: f.run.ID, RequestID: "setup-1", Status: "resolved", ActionCallID: "setup-call"}
		if mutate != nil {
			mutate(r)
		}
		return []AgentRunIntervention{{ID: "i1", SkillSetupResolution: r}}
	}
	if plan := finalAnswerPark(f.run, f.views(), false, "done", receipt(nil)); plan != nil {
		t.Fatalf("a resolved setup must not park, got %+v", plan)
	}
	if plan := finalAnswerPark(f.run, f.views(), false, "done", receipt(func(r *SkillSetupResolutionReceipt) { r.Status = "dismissed" })); plan != nil {
		t.Fatalf("a dismissed setup must not park, got %+v", plan)
	}
	for name, mutate := range map[string]func(*SkillSetupResolutionReceipt){
		"other request": func(r *SkillSetupResolutionReceipt) { r.RequestID = "setup-9" },
		"other run":     func(r *SkillSetupResolutionReceipt) { r.RunID = "run-else" },
		"other call":    func(r *SkillSetupResolutionReceipt) { r.ActionCallID = "call-else" },
		"other scope":   func(r *SkillSetupResolutionReceipt) { r.Scope = Scope{Kind: "workspace", ID: "else"} },
		"still pending": func(r *SkillSetupResolutionReceipt) { r.Status = "pending" },
		"no call id":    func(r *SkillSetupResolutionReceipt) { r.ActionCallID = "" },
	} {
		if plan := finalAnswerPark(f.run, f.views(), false, "done", receipt(mutate)); plan == nil {
			t.Fatalf("%s: a non-matching receipt must not clear the park", name)
		}
	}
	// Ordinary steering text never claims setup is finished.
	text := []AgentRunIntervention{{ID: "i2", Instruction: "I finished the setup"}}
	if plan := finalAnswerPark(f.run, f.views(), false, "done", text); plan == nil {
		t.Fatal("operator text must not clear the park")
	}
}

func TestForegroundPendingSetupAppendsNoteNotToolChange(t *testing.T) {
	f := newGuardFixture(t)
	entry := f.requestSetup("setup-call", "setup-1")
	call := f.calls["setup-call"]

	// Foreground Runs complete normally at the final answer.
	if plan := finalAnswerPark(f.run, f.views(), true, "Please finish the form.", nil); plan != nil {
		t.Fatalf("a foreground Run must not park on setup, got %+v", plan)
	}
	note := pendingSkillSetupNote(f.run, f.run.SessionID, entry, call, nil)
	if note == nil {
		t.Fatal("expected a setup note after the request_setup entry")
	}
	if err := note.Validate(); err != nil {
		t.Fatalf("note does not validate: %v", err)
	}
	key := SkillSetupPendingSourceKey(f.run.ID, "setup-1")
	if note.Type != SessionEntryNote || note.Note.Kind != NoteSetupPending || note.SourceKey != key ||
		note.ID != SessionEntryID(f.run.SessionID, key) || note.RunID != f.run.ID || note.StepID != entry.StepID {
		t.Fatalf("unexpected note: %+v", note)
	}
	if !strings.Contains(note.Note.Text, "do not poll list_setup_requests") {
		t.Fatalf("unexpected note text: %q", note.Note.Text)
	}
	// Deterministic: a second evaluation gives the same idempotent entry.
	if again := pendingSkillSetupNote(f.run, f.run.SessionID, entry, call, nil); again == nil || again.ID != note.ID {
		t.Fatal("the note must be idempotent by source key")
	}
	// No note for background task Runs (they park), resolved forms, or other calls.
	task := *f.run
	task.Kind, task.Context = RunKindAgentWork, map[string]interface{}{ConversationTaskContextKey: "task-1"}
	if pendingSkillSetupNote(&task, f.run.SessionID, entry, call, nil) != nil {
		t.Fatal("background task Runs park instead of getting a note")
	}
	resolved := []AgentRunIntervention{{ID: "i", SkillSetupResolution: &SkillSetupResolutionReceipt{
		Scope: f.run.Scope, RunID: f.run.ID, RequestID: "setup-1", Status: "resolved", ActionCallID: "setup-call"}}}
	if pendingSkillSetupNote(f.run, f.run.SessionID, entry, call, resolved) != nil {
		t.Fatal("a resolved form needs no note")
	}
	other := f.add("weather", "skill-weather", "forecast", ActionCallStatusSucceeded, nil, "", nil)
	if pendingSkillSetupNote(f.run, f.run.SessionID, other, f.calls["weather"], nil) != nil {
		t.Fatal("only setup calls get the note")
	}
}

func TestGovernedToolViewsReadsRunCallsInPathOrder(t *testing.T) {
	f := newGuardFixture(t)
	f.rateLimitedLightpanda("compacted-429")
	f.path.Entries = nil // summarized by a compaction
	f.path.Compaction = &SessionEntry{Type: SessionEntryCompaction, Compaction: &CompactionEntry{ActionCallIDs: []string{"compacted-429"}}}
	f.add("mine", "skill-weather", "forecast", ActionCallStatusSucceeded, nil, "", nil)
	// Another Run's entry in the same session, an open entry and a vm call are skipped.
	foreign := f.add("foreign", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusSucceeded, nil, "", nil)
	foreign.RunID = "run-else"
	f.calls["foreign"].RunID = "run-else"
	open := f.add("open", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusSucceeded, nil, "", nil)
	open.State = ToolEntryRunning
	f.add("vm", lightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusSucceeded, nil, "", nil)
	f.calls["vm"].Origin = ActionOriginVM

	views := f.views()
	if len(views) != 2 || views[0].ActionCallID != "compacted-429" || views[1].ActionCallID != "mine" {
		t.Fatalf("unexpected views: %+v", views)
	}
	if views[0].FailureKind != "source_rate_limited" || views[0].ErrorDetails["failureKind"] != "source_rate_limited" {
		t.Fatalf("expected typed failure details, got %+v", views[0])
	}
	if !browserSourceRateLimited(views) {
		t.Fatal("a compacted rate limit still holds")
	}

	// The store-backed loader returns the same views.
	store := newTestKernelStore(t)
	for _, call := range f.calls {
		seedActionCall(t, store, call)
	}
	loaded, err := loadGovernedToolViews(context.Background(), store, f.run.Scope, f.path, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[0].ActionCallID != "compacted-429" || loaded[1].ActionCallID != "mine" {
		t.Fatalf("unexpected loaded views: %+v", loaded)
	}
}
