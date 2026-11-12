package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var testSessionScope = Scope{Kind: "org", ID: "o1"}

func testTextPart(text string) ContentPart { return ContentPart{Type: ContentPartText, Text: text} }

func testUserEntry(id, text string) *SessionEntry {
	return &SessionEntry{ID: id, Type: SessionEntryUser, User: &UserEntry{
		Origin: UserEntryChannelMessage, Author: SessionAuthor{Type: "user", ID: "u1"},
		Content: []ContentPart{testTextPart(text)}, MessageID: "m-" + id,
	}}
}

func testAssistantEntry(id, stepID string, callIDs ...string) *SessionEntry {
	entry := &SessionEntry{ID: id, StepID: stepID, Type: SessionEntryAssistant, Assistant: &AssistantEntry{
		Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopEnd, Text: "ok",
	}}
	for _, callID := range callIDs {
		entry.Assistant.ToolCalls = append(entry.Assistant.ToolCalls, ModelToolCall{ID: callID, Name: "linear_get_issue", Arguments: json.RawMessage(`{"id":"1"}`)})
		entry.Assistant.StopReason = ModelStopToolCalls
	}
	return entry
}

func testToolEntry(id, stepID, callID string, state ToolEntryState) *SessionEntry {
	entry := &SessionEntry{ID: id, StepID: stepID, Type: SessionEntryTool, State: state, ToolCallID: callID, Tool: &ToolEntry{
		Name: "linear_get_issue", Arguments: json.RawMessage(`{"id":"1"}`), SideEffect: ToolSideEffectRead,
	}}
	switch {
	case state.Terminal():
		status := ToolResultSucceeded
		if state == ToolEntryError {
			status = ToolResultFailed
		}
		if state == ToolEntryInterrupted {
			status = ToolResultInterrupted
		}
		entry.Tool.Result = &ToolResult{Status: status, IsError: status != ToolResultSucceeded, Content: []ContentPart{testTextPart("done")}}
	case state.Awaiting():
		entry.Tool.Wait = &ToolEntryWait{ApprovalID: "ap1"}
	}
	return entry
}

func TestValidToolEntryTransitionTable(t *testing.T) {
	allowed := map[ToolEntryState]map[ToolEntryState]bool{
		ToolEntryPending: {ToolEntryRunning: true, ToolEntryAwaitingApproval: true, ToolEntryAwaitingChild: true,
			ToolEntryAwaitingEvent: true, ToolEntryCompleted: true, ToolEntryError: true, ToolEntryInterrupted: true},
		ToolEntryAwaitingApproval: {ToolEntryRunning: true, ToolEntryError: true, ToolEntryInterrupted: true},
		ToolEntryAwaitingChild:    {ToolEntryCompleted: true, ToolEntryError: true, ToolEntryInterrupted: true},
		ToolEntryAwaitingEvent:    {ToolEntryRunning: true, ToolEntryCompleted: true, ToolEntryError: true, ToolEntryInterrupted: true},
		ToolEntryRunning: {ToolEntryPending: true, ToolEntryAwaitingApproval: true, ToolEntryAwaitingChild: true,
			ToolEntryAwaitingEvent: true, ToolEntryCompleted: true, ToolEntryError: true, ToolEntryInterrupted: true},
	}
	states := append([]ToolEntryState{"", "bogus"}, ToolEntryStates...)
	for _, from := range states {
		for _, to := range states {
			want := allowed[from][to]
			if got := ValidToolEntryTransition(from, to); got != want {
				t.Errorf("ValidToolEntryTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, state := range ToolEntryStates {
		if state.Terminal() == state.Open() {
			t.Errorf("%s: Terminal and Open must differ", state)
		}
	}
	if ToolEntryState("bogus").Open() {
		t.Error("unknown state must not be open")
	}
}

func testPathEntry(entry *SessionEntry, sessionID string, seq int64, tokens int64) *SessionEntry {
	entry.SessionID, entry.Sequence, entry.Tokens, entry.Scope = sessionID, seq, tokens, testSessionScope
	return entry
}

func pathIDs(path *AgentSessionPath) string {
	ids := make([]string, 0, len(path.Entries))
	for _, entry := range path.Entries {
		ids = append(ids, entry.ID)
	}
	return strings.Join(ids, ",")
}

func TestSessionEntryFilterNormalizeAndMatch(t *testing.T) {
	if _, err := normalizeSessionEntryFilter(SessionEntryFilter{Scope: testSessionScope}); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := normalizeSessionEntryFilter(SessionEntryFilter{Scope: testSessionScope, RunID: "r", AfterSequence: 2}); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("afterSequence without session: %v", err)
	}
	filter, err := normalizeSessionEntryFilter(SessionEntryFilter{Scope: testSessionScope, RunID: "r1", States: OpenToolEntryStates, Limit: 5000})
	if err != nil || filter.Limit != maxSessionEntryListLimit {
		t.Fatalf("filter %+v err %v", filter, err)
	}
	open := testPathEntry(testToolEntry("t", "s", "c", ToolEntryRunning), "sess", 2, 1)
	open.RunID = "r1"
	done := testPathEntry(testToolEntry("t2", "s", "c2", ToolEntryCompleted), "sess", 3, 1)
	done.RunID = "r1"
	if !sessionEntryMatchesFilter(open, filter) || sessionEntryMatchesFilter(done, filter) {
		t.Fatal("open-state filter")
	}
	notes := SessionEntryFilter{Scope: testSessionScope, SessionID: "sess", NoteKinds: []NoteKind{NoteRepeat}}
	note := testPathEntry(&SessionEntry{ID: "n", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteRepeat, Text: "x"}}, "sess", 4, 1)
	if !sessionEntryMatchesFilter(note, notes) || sessionEntryMatchesFilter(open, notes) {
		t.Fatal("note kind filter")
	}
}
