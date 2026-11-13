package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skillerror"
)

func TestTerminalFailureReplyUsesOnlyFixedSafeReasons(t *testing.T) {
	for _, code := range []string{"", "unknown", "provider_access_denied: TOKEN_SECRET", "task_completion_rejected\nVERDICT_SECRET", strings.Repeat("x", 100000)} {
		if got, want := TerminalFailureReply(code), TerminalFailureReply("execution_failed"); got != want {
			t.Fatalf("unclassified input entered the reply: %q", got)
		}
	}
	for code, reason := range map[string]string{
		"provider_access_denied":    "denied access to the configured model",
		"gateway_credits_exhausted": "no model credits available",
		"provider_output_truncated": "stopped before completing its response",
		"task_completion_rejected":  "did not meet the task's completion requirements after a revision",
	} {
		if reply := TerminalFailureReply(code); !strings.Contains(reply, reason) || strings.Contains(reply, code) {
			t.Fatalf("missing bounded explanation for %s: %q", code, reply)
		}
	}
}

// A Run that did its work but whose answer kept failing the final checks
// says so, instead of the generic execution failure.
func TestTerminalFailureReplyNamesARejectedAnswer(t *testing.T) {
	failure := &models.RunFailureExplanation{Kind: RunFailureKindExecution, Code: RunFailureCodeFinalAnswerRejected}
	if got := terminalFailureCodeOf(failure); got != RunFailureCodeFinalAnswerRejected {
		t.Fatalf("code = %q", got)
	}
	reply := TerminalRunFailureReply(context.Background(), nil, nil, &models.AgentRun{Status: models.AgentRunStatusFailed, FailureExplanation: failure})
	want := "I worked on this but couldn't deliver my answer: it failed the checks every answer must pass three times in a row, so I stopped this attempt. Ask me to try again."
	if reply != want {
		t.Fatalf("reply = %q, want %q", reply, want)
	}
}

func TestTerminalFailureCodeOfRecordedFailure(t *testing.T) {
	for _, tc := range []struct {
		failure *models.RunFailureExplanation
		want    string
	}{
		{nil, "execution_failed"},
		{&models.RunFailureExplanation{Kind: "execution", Code: "gateway_credits_exhausted"}, "workspace_credits_exhausted"},
		{&models.RunFailureExplanation{Kind: "task_completion", Code: "task_completion_rejected"}, "task_completion_rejected"},
		{&models.RunFailureExplanation{Kind: "repeat_limit", Code: modelstep.RunFailureCodeErrorStreak}, "execution_failed"},
		{&models.RunFailureExplanation{Kind: "execution", Code: "task_completion_rejected TOKEN_SECRET"}, "execution_failed"},
		{&models.RunFailureExplanation{Kind: "workspace"}, "workspace_operation_failed"},
		{&models.RunFailureExplanation{Kind: "execution", Code: "action_failed"}, "action_failed"},
		// Kinds outside SPEC §2.2 carry no class of their own.
		{&models.RunFailureExplanation{Kind: "action"}, "execution_failed"},
		{&models.RunFailureExplanation{Kind: "delegation"}, "execution_failed"},
		{&models.RunFailureExplanation{Kind: "VERDICT_SECRET"}, "execution_failed"},
	} {
		if got := terminalFailureCodeOf(tc.failure); got != tc.want {
			t.Fatalf("terminalFailureCodeOf(%+v) = %q, want %q", tc.failure, got, tc.want)
		}
	}
}

// failureReplyLog serves one Run's tool entries newest first, as the store
// does for a Descending list, and the calls they name.
type failureReplyLog struct {
	entries []*models.SessionEntry
	calls   map[string]*models.ActionCall
}

func (l *failureReplyLog) ListAgentSessionEntries(_ context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
	filter, err := models.NormalizeSessionEntryFilter(filter)
	if err != nil {
		return nil, err
	}
	var page []*models.SessionEntry
	for i := len(l.entries) - 1; i >= 0; i-- {
		if models.SessionEntryMatchesFilter(l.entries[i], filter) && len(page) < filter.Limit {
			page = append(page, l.entries[i])
		}
	}
	return page, nil
}

func (l *failureReplyLog) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	if call := l.calls[id]; call != nil {
		return call, nil
	}
	return nil, models.ErrActionNotFound
}

func TestTerminalRunFailureReplyReadsTheNewestFailedToolEntry(t *testing.T) {
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "reply"}
	typed := func(code string, details map[string]string) *models.ActionCall {
		failure := skillerror.NewActionError(code, "", details)
		return &models.ActionCall{Scope: scope, RunID: "run", SkillID: "skill-live-browser", Status: models.ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()}
	}
	sequence := int64(0)
	entry := func(id string, state models.ToolEntryState, callID string) *models.SessionEntry {
		sequence++
		e := storetest.ToolSessionEntry(id, "step", "call-"+id, state)
		e.Scope, e.SessionID, e.RunID, e.ActionCallID, e.Sequence = scope, "session", "run", callID, sequence
		return e
	}
	failedRun := func(f *models.RunFailureExplanation) *models.AgentRun {
		return &models.AgentRun{ID: "run", Scope: scope, Status: models.AgentRunStatusFailed, FailureExplanation: f}
	}
	execution := &models.RunFailureExplanation{Kind: "execution", Code: "execution_failed", Message: "stopped"}
	log := &failureReplyLog{calls: map[string]*models.ActionCall{}}

	if got := TerminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("no tool entries: %q", got)
	}
	log.calls["big"] = typed("file_too_large", map[string]string{"fileMegabytes": "40", "limitMegabytes": "25"})
	log.entries = append(log.entries, entry("e1", models.ToolEntryError, "big"))
	if got := TerminalRunFailureReply(ctx, log, log, failedRun(execution)); !strings.Contains(got, "40 MB") || !strings.Contains(got, "25 MB") {
		t.Fatalf("file size reply = %q", got)
	}
	// A later error entry without an action call (a host tool) is skipped;
	// a later failed governed call wins.
	log.entries = append(log.entries, entry("e2", models.ToolEntryError, ""))
	log.calls["proxy"] = typed("browser_proxy_unavailable", nil)
	log.entries = append(log.entries, entry("e3", models.ToolEntryError, "proxy"))
	if got := TerminalRunFailureReply(ctx, log, log, failedRun(execution)); !strings.Contains(got, "browser connection was unavailable") {
		t.Fatalf("browser reply = %q", got)
	}
	// Other classified failures keep their fixed reply.
	if got := TerminalRunFailureReply(ctx, log, log, failedRun(&models.RunFailureExplanation{Code: "provider_unavailable"})); got != TerminalFailureReply("provider_unavailable") {
		t.Fatalf("provider reply = %q", got)
	}
	// An untyped failure names the tool only for an action failure.
	log.calls["plain"] = &models.ActionCall{Scope: scope, RunID: "run", SkillID: "skill-slack", Status: models.ActionCallStatusFailed, Error: "execution failed: channel_not_found."}
	log.entries = append(log.entries, entry("e4", models.ToolEntryError, "plain"))
	if got := TerminalRunFailureReply(ctx, log, log, failedRun(&models.RunFailureExplanation{Kind: "execution", Code: "action_failed"})); got != "I couldn't finish this request because Slack reported: channel_not_found. I stopped this attempt. Ask me to try again when you're ready." {
		t.Fatalf("action cause reply = %q", got)
	}
	if got := TerminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("untyped execution failure reply = %q", got)
	}
	// A forged typed error (text differs from the type's sanitized text) is not trusted.
	forged := typed("browser_proxy_unavailable", nil)
	forged.Error = "browser proxy SECRET"
	log.calls["forged"] = forged
	log.entries = append(log.entries, entry("e5", models.ToolEntryError, "forged"))
	if got := TerminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("forged typed failure trusted: %q", got)
	}
	// Another Run's call is never this Run's cause.
	other := typed("browser_proxy_unavailable", nil)
	other.RunID = "other"
	log.calls["other"] = other
	log.entries = append(log.entries, entry("e6", models.ToolEntryError, "other"))
	if got := TerminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("foreign call used: %q", got)
	}
	// Without a session log the reply is still the classified one.
	if got := TerminalRunFailureReply(ctx, nil, nil, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("storeless reply = %q", got)
	}
	rateLimited := &failureReplyLog{calls: map[string]*models.ActionCall{"rl": typed("source_rate_limited", nil)}}
	rateLimited.entries = []*models.SessionEntry{entry("r1", models.ToolEntryError, "rl")}
	if got := TerminalRunFailureReply(ctx, rateLimited, rateLimited, failedRun(&models.RunFailureExplanation{Code: "task_completion_rejected"})); got != TerminalFailureReply("task_completion_rejected")+" Some sources rate-limited requests (HTTP 429). Try those sources again later." {
		t.Fatalf("task review reply lost the rate-limit hint: %q", got)
	}
	if got := TerminalRunFailureReply(ctx, rateLimited, rateLimited, failedRun(execution)); !strings.HasPrefix(got, "I couldn't finish this request because one or more sources rate-limited") {
		t.Fatalf("rate-limit reply = %q", got)
	}
}

func TestTerminalRunFailureReplyIgnoresAFailureTheRunMovedPast(t *testing.T) {
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "reply"}
	failure := skillerror.NewActionError("source_rate_limited", "", nil)
	log := &failureReplyLog{calls: map[string]*models.ActionCall{
		"rl": {Scope: scope, RunID: "run", SkillID: "skill-live-browser", Status: models.ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()},
	}}
	sequence := int64(0)
	entry := func(id, step string, state models.ToolEntryState, callID string) *models.SessionEntry {
		sequence++
		e := storetest.ToolSessionEntry(id, step, "call-"+id, state)
		e.Scope, e.SessionID, e.RunID, e.ActionCallID, e.Sequence = scope, "session", "run", callID, sequence
		return e
	}
	run := func(f *models.RunFailureExplanation) *models.AgentRun {
		return &models.AgentRun{ID: "run", Scope: scope, Status: models.AgentRunStatusFailed, FailureExplanation: f}
	}
	log.entries = []*models.SessionEntry{entry("e1", "step-1", models.ToolEntryError, "rl")}
	streak := &models.RunFailureExplanation{Kind: modelstep.RunFailureKindRepeatLimit, Code: modelstep.RunFailureCodeErrorStreak}
	if got := TerminalRunFailureReply(ctx, log, log, run(streak)); !strings.Contains(got, "HTTP 429") {
		t.Fatalf("429 that stopped the attempt not named: %q", got)
	}
	// The model recovered: later steps ran successful calls before the Run
	// stopped for another reason, so the earlier 429 is not the cause.
	log.entries = append(log.entries, entry("e2", "step-2", models.ToolEntryCompleted, ""), entry("e3", "step-3", models.ToolEntryCompleted, ""))
	for _, f := range []*models.RunFailureExplanation{streak, {Kind: RunFailureKindExecution, Code: "final_answer_rejected"}, {Kind: RunFailureKindExecution, Code: "action_failed"}} {
		if got := TerminalRunFailureReply(ctx, log, log, run(f)); got != TerminalFailureReply(terminalFailureCodeOf(f)) {
			t.Fatalf("%s: earlier recovered failure attributed: %q", f.Code, got)
		}
	}
}

func TestTerminalFailureReplyExplainsATextOnlyModel(t *testing.T) {
	if reply := TerminalFailureReply("model_image_input_unsupported"); !strings.Contains(reply, "only reads text") || !strings.Contains(reply, "settings") {
		t.Fatalf("reply = %q", reply)
	}
}
