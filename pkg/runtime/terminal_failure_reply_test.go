package runtime

import (
	"context"
	"strings"
	"testing"

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

func TestTerminalFailureCodeOfRecordedFailure(t *testing.T) {
	for _, tc := range []struct {
		failure *RunFailureExplanation
		want    string
	}{
		{nil, "execution_failed"},
		{&RunFailureExplanation{Kind: "execution", Code: "gateway_credits_exhausted"}, "workspace_credits_exhausted"},
		{&RunFailureExplanation{Kind: "task_completion", Code: "task_completion_rejected"}, "task_completion_rejected"},
		{&RunFailureExplanation{Kind: "repeat_limit", Code: RunFailureCodeErrorStreak}, "execution_failed"},
		{&RunFailureExplanation{Kind: "execution", Code: "task_completion_rejected TOKEN_SECRET"}, "execution_failed"},
		{&RunFailureExplanation{Kind: "workspace"}, "workspace_operation_failed"},
		{&RunFailureExplanation{Kind: "execution", Code: "action_failed"}, "action_failed"},
		// Kinds outside SPEC §2.2 carry no class of their own.
		{&RunFailureExplanation{Kind: "action"}, "execution_failed"},
		{&RunFailureExplanation{Kind: "delegation"}, "execution_failed"},
		{&RunFailureExplanation{Kind: "VERDICT_SECRET"}, "execution_failed"},
	} {
		if got := terminalFailureCodeOf(tc.failure); got != tc.want {
			t.Fatalf("terminalFailureCodeOf(%+v) = %q, want %q", tc.failure, got, tc.want)
		}
	}
}

// failureReplyLog serves one Run's tool entries newest first, as the store
// does for a Descending list, and the calls they name.
type failureReplyLog struct {
	entries []*SessionEntry
	calls   map[string]*ActionCall
}

func (l *failureReplyLog) ListAgentSessionEntries(_ context.Context, filter SessionEntryFilter) ([]*SessionEntry, error) {
	filter, err := normalizeSessionEntryFilter(filter)
	if err != nil {
		return nil, err
	}
	var page []*SessionEntry
	for i := len(l.entries) - 1; i >= 0; i-- {
		if sessionEntryMatchesFilter(l.entries[i], filter) && len(page) < filter.Limit {
			page = append(page, l.entries[i])
		}
	}
	return page, nil
}

func (l *failureReplyLog) GetActionCall(_ context.Context, _ Scope, id string) (*ActionCall, error) {
	if call := l.calls[id]; call != nil {
		return call, nil
	}
	return nil, ErrActionNotFound
}

func TestTerminalRunFailureReplyReadsTheNewestFailedToolEntry(t *testing.T) {
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "reply"}
	typed := func(code string, details map[string]string) *ActionCall {
		failure := skillerror.NewActionError(code, "", details)
		return &ActionCall{Scope: scope, RunID: "run", SkillID: "skill-live-browser", Status: ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()}
	}
	sequence := int64(0)
	entry := func(id string, state ToolEntryState, callID string) *SessionEntry {
		sequence++
		e := testToolEntry(id, "step", "call-"+id, state)
		e.Scope, e.SessionID, e.RunID, e.ActionCallID, e.Sequence = scope, "session", "run", callID, sequence
		return e
	}
	failedRun := func(f *RunFailureExplanation) *AgentRun {
		return &AgentRun{ID: "run", Scope: scope, Status: AgentRunStatusFailed, FailureExplanation: f}
	}
	execution := &RunFailureExplanation{Kind: "execution", Code: "execution_failed", Message: "stopped"}
	log := &failureReplyLog{calls: map[string]*ActionCall{}}

	if got := terminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("no tool entries: %q", got)
	}
	log.calls["big"] = typed("file_too_large", map[string]string{"fileMegabytes": "40", "limitMegabytes": "25"})
	log.entries = append(log.entries, entry("e1", ToolEntryError, "big"))
	if got := terminalRunFailureReply(ctx, log, log, failedRun(execution)); !strings.Contains(got, "40 MB") || !strings.Contains(got, "25 MB") {
		t.Fatalf("file size reply = %q", got)
	}
	// A later error entry without an action call (a host tool) is skipped;
	// a later failed governed call wins.
	log.entries = append(log.entries, entry("e2", ToolEntryError, ""))
	log.calls["proxy"] = typed("browser_proxy_unavailable", nil)
	log.entries = append(log.entries, entry("e3", ToolEntryError, "proxy"))
	if got := terminalRunFailureReply(ctx, log, log, failedRun(execution)); !strings.Contains(got, "browser connection was unavailable") {
		t.Fatalf("browser reply = %q", got)
	}
	// Other classified failures keep their fixed reply.
	if got := terminalRunFailureReply(ctx, log, log, failedRun(&RunFailureExplanation{Code: "provider_unavailable"})); got != TerminalFailureReply("provider_unavailable") {
		t.Fatalf("provider reply = %q", got)
	}
	// An untyped failure names the tool only for an action failure.
	log.calls["plain"] = &ActionCall{Scope: scope, RunID: "run", SkillID: "skill-slack", Status: ActionCallStatusFailed, Error: "execution failed: channel_not_found."}
	log.entries = append(log.entries, entry("e4", ToolEntryError, "plain"))
	if got := terminalRunFailureReply(ctx, log, log, failedRun(&RunFailureExplanation{Kind: "execution", Code: "action_failed"})); got != "I couldn't finish this request because Slack reported: channel_not_found. I stopped this attempt. Ask me to try again when you're ready." {
		t.Fatalf("action cause reply = %q", got)
	}
	if got := terminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("untyped execution failure reply = %q", got)
	}
	// A forged typed error (text differs from the type's sanitized text) is not trusted.
	forged := typed("browser_proxy_unavailable", nil)
	forged.Error = "browser proxy SECRET"
	log.calls["forged"] = forged
	log.entries = append(log.entries, entry("e5", ToolEntryError, "forged"))
	if got := terminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("forged typed failure trusted: %q", got)
	}
	// Another Run's call is never this Run's cause.
	other := typed("browser_proxy_unavailable", nil)
	other.RunID = "other"
	log.calls["other"] = other
	log.entries = append(log.entries, entry("e6", ToolEntryError, "other"))
	if got := terminalRunFailureReply(ctx, log, log, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("foreign call used: %q", got)
	}
	// Without a session log the reply is still the classified one.
	if got := terminalRunFailureReply(ctx, nil, nil, failedRun(execution)); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("storeless reply = %q", got)
	}
	rateLimited := &failureReplyLog{calls: map[string]*ActionCall{"rl": typed("source_rate_limited", nil)}}
	rateLimited.entries = []*SessionEntry{entry("r1", ToolEntryError, "rl")}
	if got := terminalRunFailureReply(ctx, rateLimited, rateLimited, failedRun(&RunFailureExplanation{Code: "task_completion_rejected"})); got != TerminalFailureReply("task_completion_rejected")+" Some sources rate-limited requests (HTTP 429). Try those sources again later." {
		t.Fatalf("task review reply lost the rate-limit hint: %q", got)
	}
	if got := terminalRunFailureReply(ctx, rateLimited, rateLimited, failedRun(execution)); !strings.HasPrefix(got, "I couldn't finish this request because one or more sources rate-limited") {
		t.Fatalf("rate-limit reply = %q", got)
	}
}

func TestTerminalRunFailureReplyIgnoresAFailureTheRunMovedPast(t *testing.T) {
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "reply"}
	failure := skillerror.NewActionError("source_rate_limited", "", nil)
	log := &failureReplyLog{calls: map[string]*ActionCall{
		"rl": {Scope: scope, RunID: "run", SkillID: "skill-live-browser", Status: ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()},
	}}
	sequence := int64(0)
	entry := func(id, step string, state ToolEntryState, callID string) *SessionEntry {
		sequence++
		e := testToolEntry(id, step, "call-"+id, state)
		e.Scope, e.SessionID, e.RunID, e.ActionCallID, e.Sequence = scope, "session", "run", callID, sequence
		return e
	}
	run := func(f *RunFailureExplanation) *AgentRun {
		return &AgentRun{ID: "run", Scope: scope, Status: AgentRunStatusFailed, FailureExplanation: f}
	}
	log.entries = []*SessionEntry{entry("e1", "step-1", ToolEntryError, "rl")}
	streak := &RunFailureExplanation{Kind: RunFailureKindRepeatLimit, Code: RunFailureCodeErrorStreak}
	if got := terminalRunFailureReply(ctx, log, log, run(streak)); !strings.Contains(got, "HTTP 429") {
		t.Fatalf("429 that stopped the attempt not named: %q", got)
	}
	// The model recovered: later steps ran successful calls before the Run
	// stopped for another reason, so the earlier 429 is not the cause.
	log.entries = append(log.entries, entry("e2", "step-2", ToolEntryCompleted, ""), entry("e3", "step-3", ToolEntryCompleted, ""))
	for _, f := range []*RunFailureExplanation{streak, {Kind: RunFailureKindExecution, Code: "final_answer_rejected"}, {Kind: RunFailureKindExecution, Code: "action_failed"}} {
		if got := terminalRunFailureReply(ctx, log, log, run(f)); got != TerminalFailureReply(terminalFailureCodeOf(f)) {
			t.Fatalf("%s: earlier recovered failure attributed: %q", f.Code, got)
		}
	}
}

func TestTerminalFailureReplyExplainsATextOnlyModel(t *testing.T) {
	if reply := TerminalFailureReply("model_image_input_unsupported"); !strings.Contains(reply, "only reads text") || !strings.Contains(reply, "settings") {
		t.Fatalf("reply = %q", reply)
	}
}
