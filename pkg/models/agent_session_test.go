package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestToolEntryTransitionRequestRules(t *testing.T) {
	base := ToolEntryTransitionRequest{Scope: testSessionScope, EntryID: "e1", RunID: "r1", Lease: AgentRunLeaseGuard{WorkerID: "w1"}}
	ok := func(from, to ToolEntryState, wait *ToolEntryWait, result *ToolResult) ToolEntryTransitionRequest {
		req := base
		req.From, req.To, req.Wait, req.Result = from, to, wait, result
		return req
	}
	succeeded := &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{testTextPart("x")}}
	cases := []struct {
		name  string
		req   ToolEntryTransitionRequest
		valid bool
	}{
		{"pending->running", ok(ToolEntryPending, ToolEntryRunning, nil, nil), true},
		{"running->completed", ok(ToolEntryRunning, ToolEntryCompleted, nil, succeeded), true},
		{"running->awaiting_approval", ok(ToolEntryRunning, ToolEntryAwaitingApproval, &ToolEntryWait{ApprovalID: "a"}, nil), true},
		{"awaiting without wait", ok(ToolEntryRunning, ToolEntryAwaitingApproval, nil, nil), false},
		{"terminal without result", ok(ToolEntryRunning, ToolEntryCompleted, nil, nil), false},
		{"result on non-terminal", ok(ToolEntryPending, ToolEntryRunning, nil, succeeded), false},
		{"isError disagrees", ok(ToolEntryRunning, ToolEntryError, nil, &ToolResult{Status: ToolResultFailed}), false},
		{"succeeded with error", ok(ToolEntryRunning, ToolEntryError, nil, succeeded), false},
		{"interrupted needs interrupted status", ok(ToolEntryRunning, ToolEntryInterrupted, nil, &ToolResult{Status: ToolResultFailed, IsError: true}), false},
		{"interrupted canceled", ok(ToolEntryRunning, ToolEntryInterrupted, nil, &ToolResult{Status: ToolResultCanceled, IsError: true}), true},
		{"completed->running", ok(ToolEntryCompleted, ToolEntryRunning, nil, nil), false},
		{"pending->pending", ok(ToolEntryPending, ToolEntryPending, nil, nil), false},
		{"awaiting_child->running", ok(ToolEntryAwaitingChild, ToolEntryRunning, nil, nil), false},
	}
	for _, tc := range cases {
		err := ValidateToolEntryTransitionRequest(tc.req)
		if tc.valid && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.valid && !errors.Is(err, ErrInvalidToolEntryTransition) {
			t.Errorf("%s: err = %v, want ErrInvalidToolEntryTransition", tc.name, err)
		}
	}
}

func TestApplyToolEntryTransition(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	entry := testToolEntry("e1", "s1", "c1", ToolEntryPending)
	entry.RunID, entry.Revision = "r1", 1
	req := ToolEntryTransitionRequest{Scope: testSessionScope, EntryID: "e1", RunID: "r1", Lease: AgentRunLeaseGuard{WorkerID: "w"},
		From: ToolEntryPending, To: ToolEntryRunning, ExpectedRevision: 1, ActionCallID: "ac1", Now: now}
	parked := true
	req.Kernel = &ToolEntryKernelPatch{Parked: &parked, OriginRef: "command:x"}
	running, err := ApplyToolEntryTransition(entry, req)
	if err != nil {
		t.Fatal(err)
	}
	if running.State != ToolEntryRunning || running.Revision != 2 || running.ActionCallID != "ac1" ||
		running.Tool.StartedAt == nil || !running.Tool.Parked || running.Tool.OriginRef != "command:x" {
		t.Fatalf("unexpected running entry %+v / %+v", running, running.Tool)
	}
	if entry.State != ToolEntryPending || entry.Tool.Parked {
		t.Fatal("apply mutated its input")
	}
	if running.Tokens != EstimateSessionEntryTokens(running) {
		t.Errorf("kernel-only patch tokens = %d, want re-estimate %d", running.Tokens, EstimateSessionEntryTokens(running))
	}
	done := req
	done.From, done.To, done.ExpectedRevision, done.Kernel = ToolEntryRunning, ToolEntryCompleted, 2, nil
	done.Result = &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{testTextPart("result")}}
	completed, err := ApplyToolEntryTransition(running, done)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Revision != 3 || completed.Tool.CompletedAt == nil || completed.Tool.StartedAt == nil || completed.Tokens == 0 {
		t.Fatalf("unexpected completed entry %+v", completed)
	}
	if _, err := ApplyToolEntryTransition(running, ToolEntryTransitionRequest{RunID: "r2", From: ToolEntryRunning, To: ToolEntryCompleted, Result: done.Result}); !errors.Is(err, ErrToolEntryNotOwned) {
		t.Fatalf("foreign run: %v", err)
	}
	if _, err := ApplyToolEntryTransition(running, transitionAtRevision(done, 1)); !errors.Is(err, ErrToolEntryStateConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	other := done
	other.ActionCallID = "ac2"
	if _, err := ApplyToolEntryTransition(running, other); !errors.Is(err, ErrInvalidToolEntryTransition) {
		t.Fatalf("action call change: %v", err)
	}
}

// transitionAtRevision is r expecting the entry at revision rev.
func transitionAtRevision(r ToolEntryTransitionRequest, rev int64) ToolEntryTransitionRequest {
	r.ExpectedRevision = rev
	return r
}

func TestSessionEntryValidateCaps(t *testing.T) {
	toolWith := func(parts ...ContentPart) *SessionEntry {
		entry := testToolEntry("t1", "s1", "c1", ToolEntryCompleted)
		entry.Tool.Result.Content = parts
		return entry
	}
	image := ContentPart{Type: ContentPartImage, MediaType: "image/png", ArtifactID: "a1", SHA256: "abc"}
	file := ContentPart{Type: ContentPartFile, Path: "tool-output/c1/output.json", MimeType: "application/json", SizeBytes: 1 << 30}

	full := strings.Repeat("x", MaxToolResultContentBytes)
	if err := toolWith(testTextPart(full)).Validate(); err != nil {
		t.Fatalf("64 KiB text must pass: %v", err)
	}
	if err := toolWith(testTextPart(full + "x")).Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("64 KiB+1 text: %v", err)
	}
	half := strings.Repeat("y", MaxToolResultContentBytes/2)
	jsonValue := json.RawMessage(`"` + half + `"`)
	if err := toolWith(testTextPart(half), ContentPart{Type: ContentPartJSON, Value: jsonValue}).Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("text+json over cap: %v", err)
	}
	// Image and file parts are references and do not count toward 64 KiB.
	if err := toolWith(testTextPart(full), image, file).Validate(); err != nil {
		t.Fatalf("image and file parts must not count: %v", err)
	}
	images := []ContentPart{testTextPart("x")}
	for i := 0; i < MaxSessionEntryImageParts; i++ {
		images = append(images, image)
	}
	if err := toolWith(images...).Validate(); err != nil {
		t.Fatalf("8 images: %v", err)
	}
	if err := toolWith(append(images, image)...).Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("9 images: %v", err)
	}
	inline := image
	inline.DataBase64 = "iVBORw0KGgo="
	if err := toolWith(inline).Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("dataBase64 on a persisted tool result: %v", err)
	}
	user := testUserEntry("u1", "hello")
	user.User.Content = append(user.User.Content, inline)
	if err := user.Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("dataBase64 on a persisted user entry: %v", err)
	}
	if err := toolWith(ContentPart{Type: ContentPartImage, MediaType: "image/png"}).Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("image without reference: %v", err)
	}
	if err := toolWith(ContentPart{Type: ContentPartImage, MediaType: "image/gif", ArtifactID: "a"}).Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("gif image: %v", err)
	}
	if err := toolWith(ContentPart{Type: "audio"}).Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("unknown part: %v", err)
	}
	many := make([]ContentPart, MaxToolResultParts+1)
	for i := range many {
		many[i] = testTextPart("x")
	}
	if err := toolWith(many...).Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("too many parts: %v", err)
	}

	args := toolWith(testTextPart("x"))
	args.Tool.Arguments = json.RawMessage(`{"a":"` + strings.Repeat("z", MaxToolArgumentsBytes) + `"}`)
	if err := args.Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("256 KiB+ arguments: %v", err)
	}
	callArgs := testAssistantEntry("a1", "s1", "c1")
	callArgs.Assistant.ToolCalls[0].Arguments = args.Tool.Arguments
	if err := callArgs.Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("256 KiB+ call arguments: %v", err)
	}

	userBig := testUserEntry("u2", strings.Repeat("u", MaxUserEntryContentBytes+1))
	if err := userBig.Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("user text over cap: %v", err)
	}
	userFiles := testUserEntry("u3", "files")
	for i := 0; i <= MaxUserEntryFileParts; i++ {
		userFiles.User.Content = append(userFiles.User.Content, file)
	}
	if err := userFiles.Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("33 file parts: %v", err)
	}
	note := &SessionEntry{ID: "n1", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteHost, Text: strings.Repeat("n", MaxNoteTextBytes+1)}}
	if err := note.Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("note over cap: %v", err)
	}
	summary := &SessionEntry{ID: "c1", Type: SessionEntryCompaction, Compaction: &CompactionEntry{Summary: strings.Repeat("s", MaxCompactionSummaryBytes+1), Trigger: CompactionTriggerThreshold}}
	if err := summary.Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("summary over cap: %v", err)
	}
	native := testAssistantEntry("a2", "s1")
	native.Assistant.Native = &AssistantNative{Anthropic: []json.RawMessage{json.RawMessage(`{"type":"thinking","signature":"` + strings.Repeat("q", MaxSessionEntryBytes) + `"}`)}}
	if err := native.Validate(); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Fatalf("2 MiB+ entry: %v", err)
	}
}

func TestSessionEntryValidateShape(t *testing.T) {
	cases := map[string]*SessionEntry{
		"no payload":         {ID: "x", Type: SessionEntryUser},
		"two payloads":       {ID: "x", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteHost, Text: "t"}, ModelChange: &ModelChangeEntry{Provider: "p", Model: "m"}},
		"payload mismatch":   {ID: "x", Type: SessionEntryNote, ModelChange: &ModelChangeEntry{Provider: "p", Model: "m"}},
		"unknown note kind":  {ID: "x", Type: SessionEntryNote, Note: &NoteEntry{Kind: "chat", Text: "t"}},
		"assistant no step":  {ID: "x", Type: SessionEntryAssistant, Assistant: &AssistantEntry{Provider: ModelStepProviderChat, Model: "m", StopReason: ModelStopEnd}},
		"user starts w file": {ID: "x", Type: SessionEntryUser, User: &UserEntry{Origin: UserEntryGoal, Author: SessionAuthor{Type: "service"}, Content: []ContentPart{{Type: ContentPartFile, Path: "p"}}}},
		"state on note":      {ID: "x", Type: SessionEntryNote, State: ToolEntryPending, Note: &NoteEntry{Kind: NoteHost, Text: "t"}},
	}
	for name, entry := range cases {
		if err := entry.Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	pendingWithResult := testToolEntry("t", "s", "c", ToolEntryPending)
	pendingWithResult.Tool.Result = &ToolResult{Status: ToolResultSucceeded}
	if err := pendingWithResult.Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Errorf("pending with result: %v", err)
	}
	directive := testToolEntry("t", "s", "c", ToolEntryPending)
	directive.Tool.Directive = json.RawMessage(`{"kind":"runbook"}`)
	if err := directive.Validate(); err != nil {
		t.Errorf("directive tool entry: %v", err)
	}
	directive.StepID = ""
	if err := directive.Validate(); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Errorf("directive tool entry without step: %v", err)
	}
}

func TestValidateAppendAgentSessionEntries(t *testing.T) {
	lease := &AgentRunLeaseGuard{WorkerID: "w1"}
	req := func(entries ...*SessionEntry) AppendAgentSessionEntriesRequest {
		return AppendAgentSessionEntriesRequest{Scope: testSessionScope, SessionID: "sess", RunID: "r1", Lease: lease, Entries: entries}
	}
	step := []*SessionEntry{testAssistantEntry("a1", "s1", "c1", "c2"), testToolEntry("t1", "s1", "c1", ToolEntryPending), testToolEntry("t2", "s1", "c2", ToolEntryPending)}
	if err := ValidateAppendAgentSessionEntries(req(step...)); err != nil {
		t.Fatalf("one step: %v", err)
	}
	bad := map[string]AppendAgentSessionEntriesRequest{
		"tool alone":          req(testToolEntry("t1", "s1", "c1", ToolEntryPending)),
		"unknown call":        req(testAssistantEntry("a1", "s1", "c1"), testToolEntry("t1", "s1", "c1", ToolEntryPending), testToolEntry("t9", "s1", "c9", ToolEntryPending)),
		"missing tool entry":  req(step[0], step[1]),
		"tool before":         req(step[1], step[0], step[2]),
		"running on append":   req(testAssistantEntry("a1", "s1", "c1"), testToolEntry("t1", "s1", "c1", ToolEntryRunning)),
		"duplicate id":        req(testUserEntry("u1", "a"), testUserEntry("u1", "b")),
		"empty batch":         req(),
		"foreign run":         req(&SessionEntry{ID: "n", RunID: "r2", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteHost, Text: "x"}}),
		"lease-less non-note": {Scope: testSessionScope, SessionID: "sess", RunID: "r1", Entries: []*SessionEntry{testUserEntry("u1", "a")}},
	}
	dupKey := req(testUserEntry("u1", "a"), testUserEntry("u2", "b"))
	dupKey.Entries[0].SourceKey, dupKey.Entries[1].SourceKey = "message:m", "message:m"
	bad["duplicate source key"] = dupKey
	for name, r := range bad {
		if err := ValidateAppendAgentSessionEntries(r); !errors.Is(err, ErrInvalidSessionEntry) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	directiveTool := func(id, stepID, callID string) *SessionEntry {
		entry := testToolEntry(id, stepID, callID, ToolEntryPending)
		entry.Tool.Directive = json.RawMessage(`{"kind":"runbook"}`)
		return entry
	}
	if err := ValidateAppendAgentSessionEntries(req(directiveTool("d1", "s1", "c1"), directiveTool("d2", "s1", "c2"))); err != nil {
		t.Errorf("directive entries with step and no assistant: %v", err)
	}
	directiveBad := map[string]AppendAgentSessionEntriesRequest{
		"directive after assistant":  req(testAssistantEntry("a1", "s1", "c1"), testToolEntry("t1", "s1", "c1", ToolEntryPending), directiveTool("d1", "s1", "c2")),
		"assistant after directive":  req(directiveTool("d1", "s1", "c1"), testAssistantEntry("a1", "s1", "c2"), testToolEntry("t1", "s1", "c2", ToolEntryPending)),
		"directive duplicate call":   req(directiveTool("d1", "s1", "c1"), directiveTool("d2", "s1", "c1")),
		"no directive, no assistant": req(testToolEntry("t1", "s1", "c1", ToolEntryPending)),
	}
	for name, r := range directiveBad {
		if err := ValidateAppendAgentSessionEntries(r); !errors.Is(err, ErrInvalidSessionEntry) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	refused := testToolEntry("t1", "s1", "c1", ToolEntryError)
	refused.Tool.Result = &ToolResult{Status: ToolResultRefused, IsError: true, Content: []ContentPart{testTextPart("arguments larger than 256 KiB")}}
	if err := ValidateAppendAgentSessionEntries(req(testAssistantEntry("a1", "s1", "c1"), refused)); err != nil {
		t.Errorf("refusal in step: %v", err)
	}
	cursor := 2
	onlyCursor := req()
	onlyCursor.ConsumeInterventionsThrough = &cursor
	if err := ValidateAppendAgentSessionEntries(onlyCursor); err != nil {
		t.Errorf("cursor-only append: %v", err)
	}
	noteOnly := req(&SessionEntry{ID: "n", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteRunFailed, Text: "failed"}})
	noteOnly.Lease = nil
	if err := ValidateAppendAgentSessionEntries(noteOnly); err != nil {
		t.Errorf("lease-less note: %v", err)
	}
	many := make([]*SessionEntry, MaxSessionAppendEntries+1)
	for i := range many {
		many[i] = testUserEntry(SessionEntryID("s", string(rune('a'+i%26))+strings.Repeat("x", i)), "x")
	}
	if err := ValidateAppendAgentSessionEntries(req(many...)); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Errorf("129 entries: %v", err)
	}
	var big []*SessionEntry
	for i := 0; i < 5; i++ {
		entry := testAssistantEntry(SessionEntryID("s", strings.Repeat("b", i+1)), "s"+strings.Repeat("b", i+1))
		entry.Assistant.Native = &AssistantNative{Responses: []json.RawMessage{json.RawMessage(`"` + strings.Repeat("r", 1900<<10) + `"`)}}
		big = append(big, entry)
	}
	if err := ValidateAppendAgentSessionEntries(req(big...)); !errors.Is(err, ErrSessionEntryTooLarge) {
		t.Errorf("8 MiB+ batch: %v", err)
	}
}

func TestSessionEntryIDDeterministicPerSession(t *testing.T) {
	a := SessionEntryID("sess-1", AssistantEntryKey("step-1"))
	if a != SessionEntryID("sess-1", AssistantEntryKey("step-1")) {
		t.Fatal("same inputs must give the same id")
	}
	if a == SessionEntryID("sess-2", AssistantEntryKey("step-1")) {
		t.Fatal("another session must give another id")
	}
	if a == SessionEntryID("sess-1", AssistantEntryKey("step-2")) {
		t.Fatal("another key must give another id")
	}
	if SessionEntryID("ab", "c") == SessionEntryID("a", "bc") {
		t.Fatal("the separator must keep session and key apart")
	}
	if ToolEntryKey("s", "c") != "tool:s:c" || ChannelMessageSourceKey("m") != "message:m" || InterventionSourceKey("i") != "intervention:i" ||
		RunContextSourceKey("r") != "run-context:r" || GoalSourceKey("r") != "goal:r" || FailureNoteSourceKey("r", "s") != "failure-note:r:s" ||
		FinalRejectionEntryKey("s") != "final-rejection:s" || TaskPromotedSourceKey("r") != "task-promoted:r" {
		t.Fatal("logical keys differ from SPEC §2.1")
	}
	if ConversationAgentSessionID(testSessionScope, "lane") != ConversationAgentSessionID(testSessionScope, "lane") ||
		ConversationAgentSessionID(testSessionScope, "lane") == ConversationAgentSessionID(Scope{Kind: "org", ID: "o2"}, "lane") ||
		RunAgentSessionID(testSessionScope, "r1") == ConversationAgentSessionID(testSessionScope, "r1") {
		t.Fatal("session ids must be stable and distinct per scope and kind")
	}
}

func testCompaction(id, sessionID string, seq int64, firstKept string) *SessionEntry {
	return testPathEntry(&SessionEntry{ID: id, Type: SessionEntryCompaction, Compaction: &CompactionEntry{
		Summary: "summary " + id, FirstKeptEntryID: firstKept, Trigger: CompactionTriggerThreshold,
	}}, sessionID, seq, 7)
}

func TestBuildAgentSessionPathOrdering(t *testing.T) {
	root := &AgentSession{ID: "root", NextSequence: 7}
	child := &AgentSession{ID: "child", ParentSessionID: "root", ForkEntryID: "p3", ForkSequence: 3, Depth: 1, NextSequence: 4}
	parent := []*SessionEntry{
		testPathEntry(testUserEntry("p4", "after fork"), "root", 4, 1),
		testPathEntry(testUserEntry("p1", "one"), "root", 1, 1),
		testPathEntry(testUserEntry("p2", "two"), "root", 2, 1),
		testPathEntry(testUserEntry("p3", "three"), "root", 3, 1),
	}
	own := []*SessionEntry{
		testPathEntry(testUserEntry("c2", "own two"), "child", 2, 1),
		testPathEntry(testUserEntry("c1", "own one"), "child", 1, 1),
		testPathEntry(testUserEntry("c9", "beyond leaf"), "child", 9, 1),
	}

	t.Run("compaction without payload", func(t *testing.T) {
		broken := testPathEntry(&SessionEntry{ID: "k", Type: SessionEntryCompaction}, "root", 5, 1)
		if _, err := BuildAgentSessionPath([]*AgentSession{root}, [][]*SessionEntry{append([]*SessionEntry{broken}, parent...)}, nil); !errors.Is(err, ErrInvalidSessionEntry) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no compaction", func(t *testing.T) {
		path, err := BuildAgentSessionPath([]*AgentSession{root}, [][]*SessionEntry{parent}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := pathIDs(path); got != "p1,p2,p3,p4" || path.Tokens != 4 || path.Compaction != nil {
			t.Fatalf("path = %s tokens %d", got, path.Tokens)
		}
	})
	t.Run("across fork", func(t *testing.T) {
		path, err := BuildAgentSessionPath([]*AgentSession{root, child}, [][]*SessionEntry{parent, own}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := pathIDs(path); got != "p1,p2,p3,c1,c2" {
			t.Fatalf("path = %s", got)
		}
		if path.Session != child || len(path.Chain) != 2 {
			t.Fatal("leaf and chain")
		}
	})
	t.Run("latest compaction with kept entries before it", func(t *testing.T) {
		entries := append([]*SessionEntry{}, parent...)
		entries = append(entries,
			testCompaction("k1", "root", 5, "p2"),
			testCompaction("k2", "root", 6, "p3"),
		)
		path, err := BuildAgentSessionPath([]*AgentSession{root}, [][]*SessionEntry{entries}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if path.Compaction == nil || path.Compaction.ID != "k2" {
			t.Fatalf("compaction = %+v", path.Compaction)
		}
		if got := pathIDs(path); got != "p3,p4" || path.Tokens != 7+2 {
			t.Fatalf("path = %s tokens %d", got, path.Tokens)
		}
	})
	t.Run("compaction keeps nothing", func(t *testing.T) {
		r := &AgentSession{ID: "root", NextSequence: 7}
		entries := append([]*SessionEntry{}, parent...)
		entries = append(entries, testCompaction("k1", "root", 5, ""), testPathEntry(testUserEntry("p6", "after"), "root", 6, 1))
		path, err := BuildAgentSessionPath([]*AgentSession{r}, [][]*SessionEntry{entries}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := pathIDs(path); got != "p6" {
			t.Fatalf("path = %s", got)
		}
	})
	t.Run("compaction in parent above fork, parent compaction after fork ignored", func(t *testing.T) {
		r := &AgentSession{ID: "root", NextSequence: 8}
		c := &AgentSession{ID: "child", ParentSessionID: "root", ForkSequence: 5, ForkEntryID: "k1", Depth: 1, NextSequence: 3}
		entries := append([]*SessionEntry{}, parent...)
		entries = append(entries, testCompaction("k1", "root", 5, "p4"), testCompaction("k-late", "root", 7, ""))
		path, err := BuildAgentSessionPath([]*AgentSession{r, c}, [][]*SessionEntry{entries, own}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if path.Compaction.ID != "k1" || pathIDs(path) != "p4,c1,c2" {
			t.Fatalf("compaction %s path %s", path.Compaction.ID, pathIDs(path))
		}
	})
	t.Run("child compaction keeps parent entries", func(t *testing.T) {
		c := &AgentSession{ID: "child", ParentSessionID: "root", ForkEntryID: "p3", ForkSequence: 3, Depth: 1, NextSequence: 4}
		ownEntries := []*SessionEntry{own[1], own[0], testCompaction("ck", "child", 3, "p2")}
		path, err := BuildAgentSessionPath([]*AgentSession{root, c}, [][]*SessionEntry{parent, ownEntries}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if path.Compaction.ID != "ck" || pathIDs(path) != "p2,p3,c1,c2" {
			t.Fatalf("compaction %s path %s", path.Compaction.ID, pathIDs(path))
		}
	})
	t.Run("kept entry off path", func(t *testing.T) {
		entries := append([]*SessionEntry{}, parent...)
		entries = append(entries, testCompaction("k1", "root", 5, "nope"))
		if _, err := BuildAgentSessionPath([]*AgentSession{root}, [][]*SessionEntry{entries}, nil); !errors.Is(err, ErrInvalidSessionEntry) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("broken chain", func(t *testing.T) {
		orphan := &AgentSession{ID: "x", ParentSessionID: "other", Depth: 1, NextSequence: 1}
		if _, err := BuildAgentSessionPath([]*AgentSession{root, orphan}, [][]*SessionEntry{nil, nil}, nil); !errors.Is(err, ErrInvalidAgentSession) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("too many entries", func(t *testing.T) {
		big := &AgentSession{ID: "root", NextSequence: MaxAgentSessionPathEntries + 2}
		var entries []*SessionEntry
		for i := int64(1); i <= MaxAgentSessionPathEntries+1; i++ {
			entries = append(entries, &SessionEntry{ID: SessionEntryID("root", string(rune(i))), SessionID: "root", Sequence: i, Type: SessionEntryNote})
		}
		if _, err := BuildAgentSessionPath([]*AgentSession{big}, [][]*SessionEntry{entries}, nil); !errors.Is(err, ErrAgentSessionPathTooLarge) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCompactionCutRespectsOpenToolEntries(t *testing.T) {
	entries := []*SessionEntry{
		testUserEntry("u1", "hello"),                                                                                      // 0
		testAssistantEntry("a1", "s1", "c1"),                                                                              // 1
		testToolEntry("t1", "s1", "c1", ToolEntryCompleted),                                                               // 2
		&SessionEntry{ID: "n1", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteHost, Text: "n"}},                      // 3
		&SessionEntry{ID: "m1", Type: SessionEntryModelChange, ModelChange: &ModelChangeEntry{Provider: "p", Model: "m"}}, // 4
		testUserEntry("u2", "again"),                                                                                      // 5
		testAssistantEntry("a2", "s2", "c2"),                                                                              // 6: another Run's in-flight step
		testToolEntry("t2", "s2", "c2", ToolEntryAwaitingApproval),                                                        // 7
		testUserEntry("u3", "more"),                                                                                       // 8
		testAssistantEntry("a3", "s3", "c3"),                                                                              // 9
		testToolEntry("t3", "s3", "c3", ToolEntryCompleted),                                                               // 10
	}
	want := map[int]bool{0: true, 1: true, 2: false, 3: true, 4: false, 5: true, 6: true, 7: false, 8: false, 9: false, 10: false, 11: false}
	for cut := 0; cut <= len(entries); cut++ {
		if got := IsValidCompactionCutAt(entries, cut); got != want[cut] {
			t.Errorf("validCompactionCutAt(cut=%d) = %v, want %v", cut, got, want[cut])
		}
	}
	if IsValidCompactionCutAt(entries, -1) || IsValidCompactionCutAt(entries, len(entries)+1) {
		t.Error("out of range cuts are invalid")
	}
	if bound := compactionOpenToolBound(entries); bound != 6 {
		t.Errorf("bound = %d, want 6", bound)
	}

	// Once the open call settles, the later cuts are allowed and keeping nothing is too.
	entries[7] = testToolEntry("t2", "s2", "c2", ToolEntryCompleted)
	if !IsValidCompactionCutAt(entries, 8) || !IsValidCompactionCutAt(entries, len(entries)) || IsValidCompactionCutAt(entries, 7) {
		t.Error("cuts after the settled call")
	}

	// The append check resolves FirstKeptEntryID against the current path.
	entries[7] = testToolEntry("t2", "s2", "c2", ToolEntryRunning)
	path := &AgentSessionPath{Entries: entries}
	compact := func(firstKept string) *SessionEntry {
		return &SessionEntry{ID: "k", Type: SessionEntryCompaction, Compaction: &CompactionEntry{Summary: "s", FirstKeptEntryID: firstKept, Trigger: CompactionTriggerThreshold}}
	}
	if err := ValidateCompactionAgainstPath(path, compact("u2")); err != nil {
		t.Errorf("cut before the open call: %v", err)
	}
	for _, kept := range []string{"u3", "t1", "", "elsewhere"} {
		if err := ValidateCompactionAgainstPath(path, compact(kept)); !errors.Is(err, ErrInvalidSessionEntry) {
			t.Errorf("firstKept %q: err = %v", kept, err)
		}
	}
}

func TestSessionEntryJSONRoundTripKeepsNativeVerbatim(t *testing.T) {
	thinking := json.RawMessage(`{"type":"thinking","thinking":"z first","signature":"EuYBCkQ","a":[3,1,2]}`)
	redacted := json.RawMessage(`{"type":"redacted_thinking","data":"ZZZ"}`)
	reasoning := json.RawMessage(`{"type":"reasoning","encrypted_content":"gAAAA","summary":[],"id":"rs_1"}`)
	entry := testAssistantEntry("a1", "s1", "c1")
	entry.Assistant.Native = &AssistantNative{
		Anthropic: []json.RawMessage{thinking, redacted}, Responses: []json.RawMessage{reasoning},
		ChatReasoningField: "reasoning_content", ChatReasoning: "  spaced\nreasoning ",
	}
	entry.Assistant.Limits = &AssistantModelLimits{ContextTokens: 200000}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SessionEntry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	native := decoded.Assistant.Native
	if !bytes.Equal(native.Anthropic[0], thinking) || !bytes.Equal(native.Anthropic[1], redacted) || !bytes.Equal(native.Responses[0], reasoning) {
		t.Fatalf("native bytes changed: %s / %s / %s", native.Anthropic[0], native.Anthropic[1], native.Responses[0])
	}
	if native.ChatReasoning != entry.Assistant.Native.ChatReasoning || native.ChatReasoningField != "reasoning_content" {
		t.Fatal("chat reasoning changed")
	}
	reencoded, err := json.Marshal(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("second encode differs:\n%s\n%s", encoded, reencoded)
	}
	cloned := CloneSessionEntry(&decoded)
	cloned.Assistant.Native.Anthropic[0][2] = 'X'
	if !bytes.Equal(decoded.Assistant.Native.Anthropic[0], thinking) {
		t.Fatal("clone shares native bytes")
	}
}

func TestEstimateSessionEntryTokens(t *testing.T) {
	note := &SessionEntry{ID: "n", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteHost, Text: "abcd"}}
	encoded, _ := json.Marshal(note.Note)
	if got, want := EstimateSessionEntryTokens(note), int64((len(encoded)+3)/4); got != want {
		t.Fatalf("tokens = %d, want %d", got, want)
	}
}

func TestAgentSessionValidate(t *testing.T) {
	conv := &AgentSession{ID: "s", Scope: testSessionScope, Kind: AgentSessionKindConversation, LaneKey: "lane", ConversationID: "c",
		AgentID: "a", RootSessionID: "s", NextSequence: 1}
	if err := conv.Validate(); err != nil {
		t.Fatal(err)
	}
	fork := &AgentSession{ID: "f", Scope: testSessionScope, Kind: AgentSessionKindRun, OwnerRunID: "r", AgentID: "a",
		ParentSessionID: "s", ForkEntryID: "e", ForkSequence: 4, RootSessionID: "s", Depth: 1, NextSequence: 1}
	if err := fork.Validate(); err != nil {
		t.Fatal(err)
	}
	deep := *fork
	deep.Depth = MaxAgentSessionDepth + 1
	if err := deep.Validate(); !errors.Is(err, ErrAgentSessionForkTooDeep) {
		t.Fatalf("deep: %v", err)
	}
	mismatch := *fork
	mismatch.ForkEntryID = ""
	if err := mismatch.Validate(); !errors.Is(err, ErrInvalidAgentSession) {
		t.Fatalf("fork mismatch: %v", err)
	}
	owned := *conv
	owned.OwnerRunID = "r"
	if err := owned.Validate(); !errors.Is(err, ErrInvalidAgentSession) {
		t.Fatalf("conversation with owner: %v", err)
	}
}

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
