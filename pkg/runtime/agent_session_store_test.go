package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Session store specifics (store-3): the Run binding fence, the watermark
// CAS, the cursor-in-append and lease-less terminal notes. The cross-backend
// conformance suite is store-4's.

var testSessionNow = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func testSessionLease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: "w1", Now: testSessionNow}
}

func testSessionLeasePtr() *AgentRunLeaseGuard {
	lease := testSessionLease()
	return &lease
}

func putTestSessionRun(t *testing.T, store *SQLiteStore, runID, sessionID string, status AgentRunStatus, interventions int) *AgentRun {
	t.Helper()
	expires := testSessionNow.Add(time.Minute)
	run := &AgentRun{
		ID: runID, Scope: testSessionScope, Status: status, Revision: 1, SessionID: sessionID,
		LeaseOwner: "w1", LeaseExpiresAt: &expires, CreatedAt: testSessionNow, UpdatedAt: testSessionNow,
	}
	for i := 0; i < interventions; i++ {
		run.PendingInterventions = append(run.PendingInterventions, AgentRunIntervention{ID: "iv" + string(rune('a'+i)), Instruction: "steer"})
	}
	// The Run is written as is, without the scheduler's rules, so a test can
	// rebind or relabel one mid-scenario.
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `DELETE FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`, testSessionScope.Kind, testSessionScope.ID, runID); err != nil {
		t.Fatal(err)
	}
	if err := insertSQLiteAgentRunConn(ctx, conn, run); err != nil {
		t.Fatal(err)
	}
	return run
}

func testStoredSessionRun(t *testing.T, store *SQLiteStore, runID string) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), testSessionScope, runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func openTestConversationSession(t *testing.T, store *SQLiteStore, runID string) *AgentSession {
	t.Helper()
	sessionID := ConversationAgentSessionID(testSessionScope, ConversationRunLane("c1", "", "agent-a"))
	putTestSessionRun(t, store, runID, sessionID, AgentRunStatusRunning, 0)
	opened, err := store.OpenConversationAgentSession(context.Background(), OpenConversationAgentSessionRequest{
		Scope: testSessionScope, ConversationID: "c1", AgentID: "agent-a", RunID: runID, Lease: testSessionLease(), Now: testSessionNow,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return opened.Session
}

func testAppend(store *SQLiteStore, sessionID, runID string, entries ...*SessionEntry) (*AppendAgentSessionEntriesResult, error) {
	return store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: sessionID, RunID: runID, Lease: testSessionLeasePtr(), Entries: entries, Now: testSessionNow,
	})
}

func TestOpenConversationSessionChecksRunBinding(t *testing.T) {
	store := newTestKernelStore(t)
	putTestSessionRun(t, store, "r1", "other-session", AgentRunStatusRunning, 0)
	req := OpenConversationAgentSessionRequest{Scope: testSessionScope, ConversationID: "c1", AgentID: "agent-a", RunID: "r1", Lease: testSessionLease()}
	if _, err := store.OpenConversationAgentSession(context.Background(), req); !errors.Is(err, ErrSessionMoved) {
		t.Fatalf("open with another binding = %v, want ErrSessionMoved", err)
	}
	bad := req
	bad.Lease.WorkerID = "w2"
	if _, err := store.OpenConversationAgentSession(context.Background(), bad); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("open with a foreign lease = %v, want ErrLeaseLost", err)
	}
	if countStoreRows(t, store, "agent_sessions") != 0 {
		t.Fatalf("a failed open stored a session")
	}

	session := openTestConversationSession(t, newTestKernelStore(t), "r2")
	if session.Kind != AgentSessionKindConversation || session.NextSequence != 1 || session.RootSessionID != session.ID || session.Revision != 1 {
		t.Fatalf("new session = %+v", session)
	}
	store = newTestKernelStore(t)
	first := openTestConversationSession(t, store, "r2")
	again, err := store.OpenConversationAgentSession(context.Background(), OpenConversationAgentSessionRequest{
		Scope: testSessionScope, ConversationID: "c1", AgentID: "agent-a", RunID: "r2", Lease: testSessionLease(),
	})
	if err != nil || again.Created || again.Session.ID != first.ID {
		t.Fatalf("second open = %+v, %v", again, err)
	}
}

func TestAppendFencedBySessionBindingAndLease(t *testing.T) {
	store := newTestKernelStore(t)
	session := openTestConversationSession(t, store, "r1")
	result, err := testAppend(store, session.ID, "r1", testUserEntry("u1", "hi"), testUserEntry("u2", "there"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if result.Session.NextSequence != 3 || result.Entries[1].Sequence != 2 || result.Entries[0].Revision != 1 || result.Entries[0].Tokens == 0 {
		t.Fatalf("append result = %+v", result)
	}

	// A Run bound to another session is fenced, even under a valid lease.
	putTestSessionRun(t, store, "r2", "elsewhere", AgentRunStatusRunning, 0)
	if _, err := testAppend(store, session.ID, "r2", testUserEntry("u3", "x")); !errors.Is(err, ErrSessionMoved) {
		t.Fatalf("append from an unbound run = %v, want ErrSessionMoved", err)
	}
	// Lane interleaving: another Run bound to the same session appends.
	putTestSessionRun(t, store, "r3", session.ID, AgentRunStatusRunning, 0)
	if _, err := testAppend(store, session.ID, "r3", testUserEntry("u4", "x")); err != nil {
		t.Fatalf("append from a second lane run: %v", err)
	}
	expired := testSessionLeasePtr()
	expired.Now = testSessionNow.Add(time.Hour)
	_, err = store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: expired, Entries: []*SessionEntry{testUserEntry("u5", "x")},
	})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease = %v, want ErrLeaseLost", err)
	}
	stored, _ := store.GetAgentSession(context.Background(), testSessionScope, session.ID)
	if stored.NextSequence != 4 {
		t.Fatalf("NextSequence = %d, want 4", stored.NextSequence)
	}
}

func TestAppendWatermarkCAS(t *testing.T) {
	store := newTestKernelStore(t)
	session := openTestConversationSession(t, store, "r1")
	zero := int64(0)
	ten := int64(10)
	entry := testUserEntry("u1", "hi")
	entry.SourceKey = ChannelMessageSourceKey("m1")
	result, err := store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(),
		Entries: []*SessionEntry{entry}, ExpectedChannelWatermark: &zero, ChannelWatermark: 10,
	})
	if err != nil || result.Session.ChannelWatermark != 10 {
		t.Fatalf("CAS append = %+v, %v", result, err)
	}
	// A stale expectation fails and writes nothing.
	_, err = store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(),
		Entries: []*SessionEntry{testUserEntry("u2", "x")}, ExpectedChannelWatermark: &zero, ChannelWatermark: 12,
	})
	if !errors.Is(err, ErrChannelWatermarkMoved) {
		t.Fatalf("stale CAS = %v, want ErrChannelWatermarkMoved", err)
	}
	if _, err := store.GetSessionEntry(context.Background(), testSessionScope, "u2"); !errors.Is(err, ErrSessionEntryNotFound) {
		t.Fatalf("stale CAS stored an entry: %v", err)
	}
	// A watermark-only append with every entry skipped still advances.
	again := testUserEntry("u3", "hi")
	again.SourceKey = ChannelMessageSourceKey("m1")
	result, err = store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(),
		Entries: []*SessionEntry{again}, ExpectedChannelWatermark: &ten, ChannelWatermark: 14,
	})
	if err != nil || len(result.Entries) != 0 || len(result.Skipped) != 1 || result.Session.ChannelWatermark != 14 || result.Session.NextSequence != 2 {
		t.Fatalf("skipped append = %+v, %v", result, err)
	}
}

func TestAppendConsumesInterventionsWithEntries(t *testing.T) {
	store := newTestKernelStore(t)
	session := openTestConversationSession(t, store, "r1")
	putTestSessionRun(t, store, "r1", session.ID, AgentRunStatusRunning, 2)
	before := testStoredSessionRun(t, store, "r1")
	through := 2
	steer := &SessionEntry{ID: "s1", Type: SessionEntryUser, SourceKey: InterventionSourceKey("iva"), User: &UserEntry{
		Origin: UserEntryIntervention, InterventionID: "iva", Author: SessionAuthor{Type: "user", ID: "u1"}, Content: []ContentPart{testTextPart("steer")},
	}}
	_, err := store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(),
		Entries: []*SessionEntry{steer}, ConsumeInterventionsThrough: &through, Now: testSessionNow,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	after := testStoredSessionRun(t, store, "r1")
	if after.ConsumedInterventions != 2 || after.Revision != before.Revision+1 {
		t.Fatalf("run after cursor append: consumed=%d revision=%d (was %d)", after.ConsumedInterventions, after.Revision, before.Revision)
	}
	// Moving the cursor backwards or past the end fails with nothing written.
	for _, bad := range []int{1, 3} {
		bad := bad
		_, err := store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
			Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(),
			Entries: []*SessionEntry{testUserEntry("bad", "x")}, ConsumeInterventionsThrough: &bad,
		})
		if !errors.Is(err, ErrInvalidSessionEntry) {
			t.Fatalf("cursor %d = %v, want ErrInvalidSessionEntry", bad, err)
		}
	}
	if _, err := store.GetSessionEntry(context.Background(), testSessionScope, "bad"); !errors.Is(err, ErrSessionEntryNotFound) {
		t.Fatalf("a rejected cursor stored its entry")
	}
	// A cursor-only append at the current value is a no-op on the Run row.
	same := 2
	if _, err := store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(), ConsumeInterventionsThrough: &same,
	}); err != nil {
		t.Fatalf("cursor-only append: %v", err)
	}
	if run := testStoredSessionRun(t, store, "r1"); run.Revision != after.Revision {
		t.Fatalf("unchanged cursor bumped the run revision")
	}
}

func TestLeaselessNoteNeedsTerminalRun(t *testing.T) {
	store := newTestKernelStore(t)
	session := openTestConversationSession(t, store, "r1")
	note := &SessionEntry{ID: "n1", Type: SessionEntryNote, SourceKey: FailureNoteSourceKey("r1", "t1"), Note: &NoteEntry{Kind: NoteRunFailed, Text: "the run failed"}}
	req := AppendAgentSessionEntriesRequest{Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Entries: []*SessionEntry{note}}
	if _, err := store.AppendAgentSessionEntries(context.Background(), req); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("lease-less append on a running run = %v, want ErrLeaseLost", err)
	}
	putTestSessionRun(t, store, "r1", session.ID, AgentRunStatusFailed, 0)
	result, err := store.AppendAgentSessionEntries(context.Background(), req)
	if err != nil || len(result.Entries) != 1 {
		t.Fatalf("lease-less note on a failed run = %+v, %v", result, err)
	}
	req.Entries = []*SessionEntry{testUserEntry("u1", "x")}
	if _, err := store.AppendAgentSessionEntries(context.Background(), req); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("lease-less non-note = %v, want ErrInvalidSessionEntry", err)
	}
}

func TestAppendReplayAndEvents(t *testing.T) {
	store := newTestKernelStore(t)
	session := openTestConversationSession(t, store, "r1")
	batch := []*SessionEntry{testAssistantEntry("a1", "t1", "c1"), testToolEntry("e1", "t1", "c1", ToolEntryPending)}
	event := &ActivityEvent{Scope: testSessionScope, RunID: "r1", EventType: "turn.narration", Summary: "Checking"}
	result, err := store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(), Entries: batch, Events: []*ActivityEvent{event},
	})
	if err != nil || len(result.Events) != 1 || result.Events[0].Sequence != 1 {
		t.Fatalf("append with event = %+v, %v", result, err)
	}
	replay, err := testAppend(store, session.ID, "r1", batch...)
	if err != nil || !replay.Replayed || len(replay.Entries) != 2 || replay.Session.NextSequence != 3 {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if _, err := testAppend(store, session.ID, "r1", testUserEntry("a1", "x"), testUserEntry("new", "y")); !errors.Is(err, ErrAgentSessionConflict) {
		t.Fatalf("partial batch = %v, want ErrAgentSessionConflict", err)
	}
	bad := &ActivityEvent{Scope: testSessionScope, RunID: "r1"}
	_, err = store.AppendAgentSessionEntries(context.Background(), AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: session.ID, RunID: "r1", Lease: testSessionLeasePtr(),
		Entries: []*SessionEntry{testUserEntry("u9", "x")}, Events: []*ActivityEvent{bad},
	})
	if !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("invalid event = %v", err)
	}
	if _, err := store.GetSessionEntry(context.Background(), testSessionScope, "u9"); !errors.Is(err, ErrSessionEntryNotFound) {
		t.Fatalf("invalid event still stored the entry")
	}
}

func TestCreateRunSessionBindsRunAndForks(t *testing.T) {
	store := newTestKernelStore(t)
	parent := openTestConversationSession(t, store, "r1")
	if _, err := testAppend(store, parent.ID, "r1", testUserEntry("p1", "a"), testUserEntry("p2", "b"), testUserEntry("p3", "c")); err != nil {
		t.Fatal(err)
	}

	// Parentless create at first claim sets run.SessionID and is idempotent.
	putTestSessionRun(t, store, "bg", "", AgentRunStatusRunning, 0)
	goal := &SessionEntry{ID: "g1", Type: SessionEntryUser, SourceKey: GoalSourceKey("bg"), User: &UserEntry{
		Origin: UserEntryGoal, GoalKind: "routine", Author: SessionAuthor{Type: "service", ID: "kernel"}, Content: []ContentPart{testTextPart("do it")},
	}}
	created, err := store.CreateRunAgentSession(context.Background(), CreateRunAgentSessionRequest{
		Scope: testSessionScope, OwnerRunID: "bg", AgentID: "agent-a", Lease: testSessionLease(), InitialEntries: []*SessionEntry{goal},
	})
	if err != nil || !created.Created || created.Session.NextSequence != 2 || created.Session.ID != RunAgentSessionID(testSessionScope, "bg") {
		t.Fatalf("create = %+v, %v", created, err)
	}
	if run := testStoredSessionRun(t, store, "bg"); run.SessionID != created.Session.ID || run.Revision != 2 {
		t.Fatalf("run binding = %q rev %d", run.SessionID, run.Revision)
	}
	again, err := store.CreateRunAgentSession(context.Background(), CreateRunAgentSessionRequest{
		Scope: testSessionScope, OwnerRunID: "bg", AgentID: "agent-a", Lease: testSessionLease(), InitialEntries: []*SessionEntry{goal},
	})
	if err != nil || again.Created || again.Session.NextSequence != 2 {
		t.Fatalf("create replay = %+v, %v", again, err)
	}
	if _, err := store.CreateRunAgentSession(context.Background(), CreateRunAgentSessionRequest{
		Scope: testSessionScope, OwnerRunID: "r1", AgentID: "agent-a", Lease: testSessionLease(),
	}); !errors.Is(err, ErrSessionMoved) {
		t.Fatalf("create for a run bound elsewhere = %v, want ErrSessionMoved", err)
	}

	// Same-Agent fork in the child-creating transaction, before p3.
	putTestSessionRun(t, store, "child", "", AgentRunStatusQueued, 0)
	_, err = createSQLiteRunSessionInOwnTx(context.Background(), store, CreateRunAgentSessionTxRequest{
		Scope: testSessionScope, OwnerRunID: "child", AgentID: "agent-a", ParentSessionID: parent.ID, ForkSequence: 5,
	})
	if !errors.Is(err, ErrInvalidAgentSession) {
		t.Fatalf("fork past the leaf = %v, want ErrInvalidAgentSession", err)
	}
	fork, err := createSQLiteRunSessionInOwnTx(context.Background(), store, CreateRunAgentSessionTxRequest{
		Scope: testSessionScope, OwnerRunID: "child", AgentID: "agent-a", ParentSessionID: parent.ID, ForkSequence: 2, Now: testSessionNow,
	})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if fork.Session.Depth != 1 || fork.Session.ForkEntryID != "p2" || fork.Session.RootSessionID != parent.ID || fork.Session.ConversationID != "c1" {
		t.Fatalf("fork session = %+v", fork.Session)
	}
	if run := testStoredSessionRun(t, store, "child"); run.SessionID != fork.Session.ID {
		t.Fatalf("child binding = %q", run.SessionID)
	}
	if _, err := testAppend(store, fork.Session.ID, "child", testUserEntry("c1", "child")); err != nil {
		t.Fatal(err)
	}
	path, err := store.LoadAgentSessionPath(context.Background(), testSessionScope, fork.Session.ID)
	if err != nil || pathIDs(path) != "p1,p2,c1" || len(path.Chain) != 2 {
		t.Fatalf("fork path = %v, %v", path, err)
	}
	// Returned paths are copies.
	path.Entries[0].User.Content[0].Text = "mutated"
	if entry, _ := store.GetSessionEntry(context.Background(), testSessionScope, "p1"); entry.User.Content[0].Text != "a" {
		t.Fatalf("path shares stored entries")
	}
}

func TestForkDepthLimit(t *testing.T) {
	store := newTestKernelStore(t)
	parent := openTestConversationSession(t, store, "r0")
	for depth := 1; depth <= MaxAgentSessionDepth+1; depth++ {
		runID := "d" + string(rune('0'+depth))
		putTestSessionRun(t, store, runID, "", AgentRunStatusQueued, 0)
		result, err := createSQLiteRunSessionInOwnTx(context.Background(), store, CreateRunAgentSessionTxRequest{
			Scope: testSessionScope, OwnerRunID: runID, AgentID: "agent-a", ParentSessionID: parent.ID,
		})
		if depth > MaxAgentSessionDepth {
			if !errors.Is(err, ErrAgentSessionForkTooDeep) {
				t.Fatalf("depth %d = %v, want ErrAgentSessionForkTooDeep", depth, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		parent = result.Session
	}
}

func TestTransitionToolEntry(t *testing.T) {
	store := newTestKernelStore(t)
	session := openTestConversationSession(t, store, "r1")
	if _, err := testAppend(store, session.ID, "r1", testAssistantEntry("a1", "t1", "c1"), testToolEntry("e1", "t1", "c1", ToolEntryPending)); err != nil {
		t.Fatal(err)
	}
	transition := func(from, to ToolEntryState, result *ToolResult, wait *ToolEntryWait) (*ToolEntryTransitionResult, error) {
		return store.TransitionToolEntry(context.Background(), ToolEntryTransitionRequest{
			Scope: testSessionScope, EntryID: "e1", RunID: "r1", Lease: testSessionLease(), From: from, To: to, Result: result, Wait: wait,
			Kernel: &ToolEntryKernelPatch{OriginRef: "command:x"}, Now: testSessionNow,
		})
	}
	if _, err := transition(ToolEntryPending, ToolEntryRunning, nil, nil); err != nil {
		t.Fatal(err)
	}
	// New edge: running -> awaiting_approval -> running.
	if _, err := transition(ToolEntryRunning, ToolEntryAwaitingApproval, nil, &ToolEntryWait{ApprovalID: "ap1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := transition(ToolEntryAwaitingApproval, ToolEntryRunning, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Another Run with a valid lease does not own the entry.
	putTestSessionRun(t, store, "r2", session.ID, AgentRunStatusRunning, 0)
	if _, err := store.TransitionToolEntry(context.Background(), ToolEntryTransitionRequest{
		Scope: testSessionScope, EntryID: "e1", RunID: "r2", Lease: testSessionLease(), From: ToolEntryRunning, To: ToolEntryInterrupted,
		Result: &ToolResult{Status: ToolResultInterrupted, IsError: true, Content: []ContentPart{testTextPart("x")}},
	}); !errors.Is(err, ErrToolEntryNotOwned) {
		t.Fatalf("foreign run = %v, want ErrToolEntryNotOwned", err)
	}

	done := &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{testTextPart("ok")}}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = transition(ToolEntryRunning, ToolEntryCompleted, done, nil)
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrToolEntryStateConflict):
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent completes: %v", errs)
	}
	entry, err := store.GetSessionEntry(context.Background(), testSessionScope, "e1")
	if err != nil || entry.State != ToolEntryCompleted || entry.Revision != 5 || entry.Tool.StartedAt == nil ||
		entry.Tool.CompletedAt == nil || entry.Tool.OriginRef != "command:x" || entry.Tool.Wait != nil {
		t.Fatalf("completed entry = %+v, %v", entry, err)
	}
	open, err := store.ListAgentSessionEntries(context.Background(), SessionEntryFilter{Scope: testSessionScope, RunID: "r1", States: OpenToolEntryStates})
	if err != nil || len(open) != 0 {
		t.Fatalf("open entries after completion = %d, %v", len(open), err)
	}
}

func TestCompactionCannotDropAssistantWithOpenToolEntry(t *testing.T) {
	store := newTestKernelStore(t)
	session := openTestConversationSession(t, store, "r1")
	if _, err := testAppend(store, session.ID, "r1", testUserEntry("u1", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := testAppend(store, session.ID, "r1", testAssistantEntry("a1", "t1", "c1"), testToolEntry("e1", "t1", "c1", ToolEntryPending)); err != nil {
		t.Fatal(err)
	}
	if _, err := testAppend(store, session.ID, "r1", testUserEntry("u2", "b")); err != nil {
		t.Fatal(err)
	}
	compaction := func(id, kept string) *SessionEntry {
		return &SessionEntry{ID: id, Type: SessionEntryCompaction, Compaction: &CompactionEntry{
			Summary: "summary", FirstKeptEntryID: kept, Trigger: CompactionTriggerThreshold, Provider: "openai", Model: "gpt-x",
		}}
	}
	if _, err := testAppend(store, session.ID, "r1", compaction("k1", "u2")); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("cut past an open call = %v, want ErrInvalidSessionEntry", err)
	}
	if _, err := testAppend(store, session.ID, "r1", compaction("k2", "e1")); !errors.Is(err, ErrInvalidSessionEntry) {
		t.Fatalf("cut at a tool entry = %v, want ErrInvalidSessionEntry", err)
	}
	if _, err := testAppend(store, session.ID, "r1", compaction("k3", "a1")); err != nil {
		t.Fatalf("cut at the open call's assistant: %v", err)
	}
	path, err := store.LoadAgentSessionPath(context.Background(), testSessionScope, session.ID)
	if err != nil || path.Compaction == nil || path.Compaction.ID != "k3" || pathIDs(path) != "a1,e1,u2" {
		t.Fatalf("path after compaction = %v, %v", path, err)
	}
	after, err := store.ListAgentSessionEntries(context.Background(), SessionEntryFilter{Scope: testSessionScope, SessionID: session.ID, AfterCompaction: true})
	if err != nil || len(after) != 0 {
		t.Fatalf("entries after compaction = %d, %v", len(after), err)
	}
}
