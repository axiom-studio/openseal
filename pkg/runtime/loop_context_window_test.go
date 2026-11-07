package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactionThreshold(t *testing.T) {
	cases := []struct {
		context, input, threshold, keep int64
	}{
		{64000, 0, 48000, 12000},
		{106667, 0, 90667, 15000},
		{200000, 0, 170000, 15000},
		{1000000, 0, 850000, 15000},
		{400000, 272000, 272000, 15000},
		{18000, 0, 0, 0},
		{0, 0, 0, 0},
		{0, 100000, 84000, 15000},
	}
	for _, tc := range cases {
		window := ModelWindow{ContextTokens: tc.context, InputLimitTokens: tc.input}
		if got := compactionThreshold(window); got != tc.threshold {
			t.Errorf("compactionThreshold(%+v) = %d, want %d", window, got, tc.threshold)
		}
		if got := compactionKeepTokens(window); got != tc.keep {
			t.Errorf("compactionKeepTokens(%+v) = %d, want %d", window, got, tc.keep)
		}
	}
	if (ModelWindow{}).Known() || !(ModelWindow{InputLimitTokens: 1}).Known() {
		t.Fatal("Known is wrong")
	}
}

func TestCompactionKeepTokens(t *testing.T) {
	// threshold/4 below 15000, capped at 15000 above.
	if got := compactionKeepTokens(ModelWindow{ContextTokens: 40000}); got != 6000 {
		t.Fatalf("keep for 40000 = %d, want 6000", got)
	}
	if got := compactionKeepTokens(ModelWindow{ContextTokens: 24000}); got != 2000 {
		t.Fatalf("keep for 24000 = %d, want 2000", got)
	}
	if got := compactionKeepTokens(ModelWindow{ContextTokens: 23999}); got != 0 {
		t.Fatalf("keep below the minimum threshold = %d, want 0", got)
	}
	if got := compactionKeepTokens(ModelWindow{ContextTokens: 2000000}); got != compactionKeepRecentTokens {
		t.Fatalf("keep for a large window = %d", got)
	}
}

func tokensFor(bytes int) int64 { return int64((bytes+3)/4) + estimatedEntryFraming }

func TestEstimateEntryTokensCountsTextToolCallsImagesReasoningNotNative(t *testing.T) {
	user := &SessionEntry{Type: SessionEntryUser, User: &UserEntry{
		Author: SessionAuthor{Type: "user", ID: "u1", DisplayName: "Ana"},
		Content: []ContentPart{
			{Type: ContentPartText, Text: strings.Repeat("a", 100)},
			{Type: ContentPartImage, MediaType: "image/png", SHA256: "x"},
			{Type: ContentPartImage, MediaType: "image/png", SHA256: "y"},
		},
	}}
	if got, want := estimateEntryTokens(user), tokensFor(len("Ana:\n")+100)+2*estimatedImageTokens; got != want {
		t.Fatalf("user tokens = %d, want %d", got, want)
	}

	args := json.RawMessage(`{"query":"weather in paris"}`)
	native := &AssistantNative{
		Anthropic: []json.RawMessage{
			json.RawMessage(`{"type":"thinking","thinking":"` + strings.Repeat("t", 40) + `","signature":"` + strings.Repeat("s", 4000) + `"}`),
			json.RawMessage(`{"type":"text","text":"` + strings.Repeat("x", 4000) + `"}`),
			json.RawMessage(`{"type":"tool_use","id":"c1","name":"search","input":{"query":"weather in paris"}}`),
		},
	}
	assistant := &SessionEntry{Type: SessionEntryAssistant, Assistant: &AssistantEntry{
		Model:     "m",
		Text:      strings.Repeat("x", 60),
		ToolCalls: []ModelToolCall{{ID: "c1", Name: "search", Arguments: args}},
		Native:    native,
		Limits:    &AssistantModelLimits{PromptTokens: 999999},
	}}
	want := tokensFor(60 + len("search") + len(args) + 40)
	if got := estimateEntryTokens(assistant); got != want {
		t.Fatalf("assistant tokens = %d, want %d", got, want)
	}
	chat := &SessionEntry{Type: SessionEntryAssistant, Assistant: &AssistantEntry{
		Text: "hi", Native: &AssistantNative{ChatReasoningField: "reasoning_content", ChatReasoning: strings.Repeat("r", 30)},
	}}
	if got, want := estimateEntryTokens(chat), tokensFor(2+30); got != want {
		t.Fatalf("chat reasoning tokens = %d, want %d", got, want)
	}
	responses := &SessionEntry{Type: SessionEntryAssistant, Assistant: &AssistantEntry{
		Native: &AssistantNative{Responses: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"abcd"},{"type":"summary_text","text":"efgh"}]}`),
			json.RawMessage(`{"type":"message","content":[{"type":"output_text","text":"` + strings.Repeat("x", 400) + `"}]}`),
		}},
	}}
	if got, want := estimateEntryTokens(responses), tokensFor(8); got != want {
		t.Fatalf("responses reasoning tokens = %d, want %d", got, want)
	}

	value := json.RawMessage(`{"ok":true}`)
	file := ContentPart{Type: ContentPartFile, Path: "out/report.csv", MimeType: "text/csv", SizeBytes: 1 << 20}
	tool := &SessionEntry{Type: SessionEntryTool, State: ToolEntryCompleted, Tool: &ToolEntry{
		Name: "search", Arguments: args, Result: &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{
			{Type: ContentPartText, Text: strings.Repeat("r", 20)},
			{Type: ContentPartJSON, Value: value},
			file,
			{Type: ContentPartImage, MediaType: "image/png", SHA256: "z"},
		}},
	}}
	want = tokensFor(20+len(value)+len(contentPartFileLine(file))) + estimatedImageTokens
	if got := estimateEntryTokens(tool); got != want {
		t.Fatalf("tool tokens = %d, want %d", got, want)
	}
	pending := &SessionEntry{Type: SessionEntryTool, State: ToolEntryRunning, Tool: &ToolEntry{Name: "search", Arguments: args}}
	if got := estimateEntryTokens(pending); got != 0 {
		t.Fatalf("open tool entry tokens = %d, want 0", got)
	}

	compaction := &CompactionEntry{Summary: "## Objective\nShip it.", ActionCallIDs: []string{"ac_1", "ac_2"}}
	if got, want := estimateEntryTokens(&SessionEntry{Type: SessionEntryCompaction, Compaction: compaction}), tokensFor(len(CompactionSummaryText(compaction))); got != want {
		t.Fatalf("compaction tokens = %d, want %d", got, want)
	}
	note := &SessionEntry{Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteRepeat, Text: strings.Repeat("n", 12), Data: json.RawMessage(`{"big":"` + strings.Repeat("d", 1000) + `"}`)}}
	if got, want := estimateEntryTokens(note), tokensFor(12); got != want {
		t.Fatalf("note tokens = %d, want %d", got, want)
	}
	change := &SessionEntry{Type: SessionEntryModelChange, ModelChange: &ModelChangeEntry{Provider: "anthropic", Model: "m2"}}
	if got := estimateEntryTokens(change); got != estimatedEntryFraming {
		t.Fatalf("model change tokens = %d, want framing only", got)
	}
	if estimateEntryTokens(nil) != 0 {
		t.Fatal("nil entry must be 0")
	}
}

func TestCompactionSummaryTextFormatInEstimate(t *testing.T) {
	text := CompactionSummaryText(&CompactionEntry{Summary: "S", ActionCallIDs: []string{"ac_1", "ac_2"}})
	want := compactionSummaryPreamble + "\n\nS\n\nAction calls recorded before this point: ac_1, ac_2"
	if text != want {
		t.Fatalf("text = %q", text)
	}
	if got := CompactionSummaryText(&CompactionEntry{Summary: "S"}); got != compactionSummaryPreamble+"\n\nS" {
		t.Fatalf("text without ids = %q", got)
	}
}

// windowPath builds a single-session path from entries with Tokens set and
// sequences assigned in order.
func windowPath(compaction *SessionEntry, entries ...*SessionEntry) *AgentSessionPath {
	session := &AgentSession{ID: "ses_1"}
	path := &AgentSessionPath{Session: session, Chain: []*AgentSession{session}, Compaction: compaction}
	if compaction != nil {
		path.Tokens += compaction.Tokens
	}
	for _, entry := range entries {
		if entry.SessionID == "" {
			entry.SessionID = "ses_1"
		}
		path.Entries = append(path.Entries, entry)
		path.Tokens += entry.Tokens
	}
	return path
}

func windowAssistant(id string, seq int64, model string, tokens, prompt int64) *SessionEntry {
	return &SessionEntry{ID: id, Sequence: seq, Type: SessionEntryAssistant, Tokens: tokens,
		Assistant: &AssistantEntry{Model: model, Limits: &AssistantModelLimits{ContextTokens: 200000, PromptTokens: prompt}}}
}

func windowUser(id string, seq, tokens int64) *SessionEntry {
	return &SessionEntry{ID: id, Sequence: seq, Type: SessionEntryUser, Tokens: tokens,
		User: &UserEntry{Origin: UserEntryChannelMessage, Content: []ContentPart{{Type: ContentPartText, Text: "x"}}}}
}

func windowTool(id string, seq, tokens int64) *SessionEntry {
	return &SessionEntry{ID: id, Sequence: seq, Type: SessionEntryTool, State: ToolEntryCompleted, Tokens: tokens,
		Tool: &ToolEntry{Name: "t", Result: &ToolResult{Status: ToolResultSucceeded}}}
}

func TestEstimatePathTokensAnchorsOnLatestPromptTokens(t *testing.T) {
	path := windowPath(nil,
		windowUser("u1", 1, 100),
		windowAssistant("a1", 2, "m", 50, 20000),
		windowTool("t1", 3, 300),
		windowAssistant("a2", 4, "m", 70, 30000),
		windowTool("t2", 5, 400),
		windowUser("u2", 6, 25),
	)
	got := estimatePathTokens(path, "m")
	if got.AnchorEntryID != "a2" || got.Tokens != 30000+70+400+25 {
		t.Fatalf("estimate = %+v", got)
	}

	// An assistant without PromptTokens is skipped; the earlier one anchors.
	path.Entries[3].Assistant.Limits.PromptTokens = 0
	got = estimatePathTokens(path, "m")
	if got.AnchorEntryID != "a1" || got.Tokens != 20000+50+300+70+400+25 {
		t.Fatalf("estimate past unanchored step = %+v", got)
	}

	// Another model never anchors.
	got = estimatePathTokens(path, "other")
	if got.AnchorEntryID != "" || got.Tokens != defaultPrefixTokens+path.Tokens {
		t.Fatalf("estimate for another model = %+v", got)
	}
	if got := estimatePathTokens(windowPath(nil), "m"); got.Tokens != defaultPrefixTokens || got.AnchorEntryID != "" {
		t.Fatalf("empty path estimate = %+v", got)
	}
}

func TestEstimatePathTokensIgnoresAnchorBeforeCompaction(t *testing.T) {
	compaction := &SessionEntry{ID: "c1", SessionID: "ses_1", Sequence: 5, Type: SessionEntryCompaction, Tokens: 900,
		Compaction: &CompactionEntry{Summary: "s"}}
	// Kept tail (sequences 3-4) is before the compaction row; its anchor
	// describes the uncompacted context.
	path := windowPath(compaction,
		windowAssistant("a1", 3, "m", 50, 180000),
		windowTool("t1", 4, 300),
		windowUser("u2", 6, 25),
	)
	got := estimatePathTokens(path, "m")
	if got.AnchorEntryID != "" || got.Tokens != defaultPrefixTokens+900+50+300+25 {
		t.Fatalf("estimate = %+v", got)
	}

	// A step after the compaction anchors.
	path = windowPath(compaction,
		windowAssistant("a1", 3, "m", 50, 180000),
		windowTool("t1", 4, 300),
		windowAssistant("a2", 6, "m", 60, 21000),
		windowTool("t2", 7, 10),
	)
	got = estimatePathTokens(path, "m")
	if got.AnchorEntryID != "a2" || got.Tokens != 21000+60+10 {
		t.Fatalf("estimate after a step = %+v", got)
	}

	// Compaction in the parent, step in the child: the child step renders after it.
	parent := &AgentSession{ID: "ses_p"}
	child := &AgentSession{ID: "ses_c", ParentSessionID: "ses_p"}
	parentCompaction := &SessionEntry{ID: "c2", SessionID: "ses_p", Sequence: 9, Type: SessionEntryCompaction, Tokens: 500,
		Compaction: &CompactionEntry{Summary: "s"}}
	childStep := windowAssistant("a3", 1, "m", 40, 12000)
	childStep.SessionID = "ses_c"
	path = &AgentSessionPath{Session: child, Chain: []*AgentSession{parent, child}, Compaction: parentCompaction,
		Entries: []*SessionEntry{childStep}, Tokens: 540}
	if got := estimatePathTokens(path, "m"); got.AnchorEntryID != "a3" || got.Tokens != 12040 {
		t.Fatalf("child estimate = %+v", got)
	}
	// Compaction in the child, step in the parent: the parent step is before it.
	parentStep := windowAssistant("a4", 3, "m", 40, 150000)
	parentStep.SessionID = "ses_p"
	childCompaction := &SessionEntry{ID: "c3", SessionID: "ses_c", Sequence: 2, Type: SessionEntryCompaction, Tokens: 500,
		Compaction: &CompactionEntry{Summary: "s"}}
	path = &AgentSessionPath{Session: child, Chain: []*AgentSession{parent, child}, Compaction: childCompaction,
		Entries: []*SessionEntry{parentStep}, Tokens: 540}
	if got := estimatePathTokens(path, "m"); got.AnchorEntryID != "" || got.Tokens != defaultPrefixTokens+540 {
		t.Fatalf("parent-before-child-compaction estimate = %+v", got)
	}
}

func TestEstimatePathTokensIgnoresAnchorAfterModelChange(t *testing.T) {
	path := windowPath(nil,
		windowAssistant("a1", 1, "m", 50, 20000),
		windowTool("t1", 2, 300),
		&SessionEntry{ID: "mc", Sequence: 3, Type: SessionEntryModelChange, Tokens: 8,
			ModelChange: &ModelChangeEntry{FromModel: "m", Provider: "anthropic", Model: "m"}},
		windowUser("u1", 4, 25),
	)
	if got := estimatePathTokens(path, "m"); got.AnchorEntryID != "" || got.Tokens != defaultPrefixTokens+path.Tokens {
		t.Fatalf("estimate = %+v", got)
	}
}

func TestPathModelWindowUsesNewestStepOfModel(t *testing.T) {
	a1 := windowAssistant("a1", 1, "m", 50, 100)
	a1.Assistant.Limits = &AssistantModelLimits{ContextTokens: 64000}
	a2 := windowAssistant("a2", 2, "n", 50, 100)
	a2.Assistant.Limits = &AssistantModelLimits{ContextTokens: 400000, InputLimitTokens: 272000}
	path := windowPath(nil, a1, a2)
	if got := pathModelWindow(path, "m"); got != (ModelWindow{ContextTokens: 64000}) {
		t.Fatalf("window m = %+v", got)
	}
	if got := pathModelWindow(path, "n"); got != (ModelWindow{ContextTokens: 400000, InputLimitTokens: 272000}) {
		t.Fatalf("window n = %+v", got)
	}
	if got := pathModelWindow(path, "new"); got.Known() {
		t.Fatalf("window for a model without steps = %+v", got)
	}
}

func TestCompactorDueSkipsSecondCompactionBeforeAStep(t *testing.T) {
	window := ModelWindow{ContextTokens: 64000} // threshold 48000
	big := windowPath(nil,
		windowUser("u1", 1, 100),
		windowAssistant("a1", 2, "m", 50, 47000),
		windowTool("t1", 3, 2000),
	)
	if !compactionDue(big, "m", window) {
		t.Fatal("over the threshold without a compaction must be due")
	}
	if compactionDue(big, "m", ModelWindow{}) {
		t.Fatal("unknown window must never be due")
	}
	small := windowPath(nil, windowAssistant("a1", 1, "m", 50, 30000))
	if compactionDue(small, "m", window) {
		t.Fatal("under the threshold must not be due")
	}

	// The kept tail alone is over the threshold right after a compaction: no
	// second threshold compaction until a step has run.
	compaction := &SessionEntry{ID: "c1", SessionID: "ses_1", Sequence: 10, Type: SessionEntryCompaction, Tokens: 2000,
		Compaction: &CompactionEntry{Summary: "s"}}
	tail := windowPath(compaction,
		windowAssistant("a5", 8, "m", 50, 60000),
		windowTool("t5", 9, 40000),
		windowUser("u6", 11, 100),
	)
	if est := estimatePathTokens(tail, "m"); est.Tokens <= compactionThreshold(window) {
		t.Fatalf("fixture must be over the threshold: %+v", est)
	}
	if compactionDue(tail, "m", window) {
		t.Fatal("second compaction before a step must not be due")
	}
	for _, entry := range []*SessionEntry{windowAssistant("a6", 12, "m", 50, 47000), windowTool("t6", 13, 2000)} {
		entry.SessionID = "ses_1"
		tail.Entries = append(tail.Entries, entry)
	}
	if !compactionDue(tail, "m", window) {
		t.Fatal("after a step over the threshold, compaction is due again")
	}
}
