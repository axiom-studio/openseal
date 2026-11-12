package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// SQLite store test: it moves to store/sqlite in U2.9. It runs the session
// conformance suite (storetest.RunAgentSessionContract) and
// the testSession* helpers of agent_session_test.go and
// agent_session_store_test.go.

// TestSQLiteAgentSessionStore runs the session store conformance suite
// (storetest.RunAgentSessionContract) against the SQLite backend.
func TestSQLiteAgentSessionStore(t *testing.T) {
	storetest.RunAgentSessionContract(t, func(t *testing.T) (storetest.AgentSessionContractStore, storetest.CreateRunSessionInOwnTx) {
		store := openTestSQLiteSessionStore(t)
		return store, func(ctx context.Context, req CreateRunAgentSessionTxRequest) (*OpenAgentSessionResult, error) {
			return createSQLiteRunSessionInOwnTx(ctx, store, req)
		}
	})
}

// createSQLiteRunSessionInOwnTx runs the store's in-transaction
// createRunAgentSessionTx helper in a write transaction of its own.
func createSQLiteRunSessionInOwnTx(ctx context.Context, s *SQLiteStore, req CreateRunAgentSessionTxRequest) (*OpenAgentSessionResult, error) {
	var result *OpenAgentSessionResult
	err := s.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		var err error
		result, err = s.createRunAgentSessionTx(ctx, conn, req)
		return err
	})
	return result, err
}

func openTestSQLiteSessionStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteAgentSessionLogMigrationReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.db")
	for i := 0; i < 2; i++ {
		store, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := migrateAgentSessionLogSQLite(store.db); err != nil {
			t.Fatalf("replay migration %d: %v", i, err)
		}
		for _, name := range []string{
			"agent_sessions", "agent_session_entries",
			"agent_sessions_lane_idx", "agent_sessions_owner_run_idx", "agent_sessions_parent_idx",
			"agent_session_entries_source_idx", "agent_session_entries_compaction_idx", "agent_session_entries_run_idx",
			"agent_session_entries_step_idx", "agent_session_entries_action_idx", "agent_session_entries_open_tool_idx",
		} {
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count); err != nil || count != 1 {
				t.Fatalf("open %d: %s present %d times (%v)", i, name, count, err)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func putTestSQLiteSessionRun(t *testing.T, store *SQLiteStore, runID, sessionID string, status AgentRunStatus) {
	t.Helper()
	ctx := context.Background()
	expires := testSessionNow.Add(time.Minute)
	run := &AgentRun{
		ID: runID, Scope: testSessionScope, Status: status, Revision: 1, SessionID: sessionID,
		LeaseOwner: "w1", LeaseExpiresAt: &expires, CreatedAt: testSessionNow, UpdatedAt: testSessionNow,
	}
	err := store.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		current, err := getSQLiteAgentRun(ctx, conn, testSessionScope, runID)
		if err != nil {
			return insertSQLiteAgentRunConn(ctx, conn, run)
		}
		run.Revision = current.Revision + 1
		return updateSQLiteAgentRunConn(ctx, conn, run, current.Revision)
	})
	if err != nil {
		t.Fatalf("put run %s: %v", runID, err)
	}
}

func TestSQLiteInTxHelpersSkipFence(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLiteSessionStore(t)
	sessionID := ConversationAgentSessionID(testSessionScope, ConversationRunLane("c1", "", "agent-a"))
	putTestSQLiteSessionRun(t, store, "r1", sessionID, AgentRunStatusRunning)
	opened, err := store.OpenConversationAgentSession(ctx, OpenConversationAgentSessionRequest{
		Scope: testSessionScope, ConversationID: "c1", AgentID: "agent-a", RunID: "r1", Lease: testSessionLease(), Now: testSessionNow,
	})
	if err != nil || !opened.Created {
		t.Fatalf("open = %+v, %v", opened, err)
	}
	if _, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: sessionID, RunID: "r1", Lease: testSessionLeasePtr(), Now: testSessionNow,
		Entries: []*SessionEntry{storetest.AssistantSessionEntry("a1", "t1", "c1"), storetest.ToolSessionEntry("e1", "t1", "c1", ToolEntryPending)},
	}); err != nil {
		t.Fatal(err)
	}
	// Promotion-style note from a Run that is now bound elsewhere, and a
	// transition under another worker: the caller's transaction is the fence.
	putTestSQLiteSessionRun(t, store, "r1", "child-session", AgentRunStatusRunning)
	note := &SessionEntry{ID: "n1", Type: SessionEntryNote, SourceKey: TaskPromotedSourceKey("r1"), Note: &NoteEntry{Kind: NoteTaskPromoted, Text: "moved", Data: json.RawMessage(`{}`)}}
	var appendErr, transitionErr error
	err = store.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		_, appendErr = store.appendAgentSessionEntriesTx(ctx, conn, AppendAgentSessionEntriesRequest{
			Scope: testSessionScope, SessionID: sessionID, RunID: "r1", Entries: []*SessionEntry{note},
		})
		_, transitionErr = store.transitionToolEntryTx(ctx, conn, ToolEntryTransitionRequest{
			Scope: testSessionScope, EntryID: "e1", RunID: "r1", Lease: AgentRunLeaseGuard{WorkerID: "w-other"},
			From: ToolEntryPending, To: ToolEntryAwaitingChild, Wait: &ToolEntryWait{ChildRunIDs: []string{"child"}},
		})
		return nil
	})
	if err != nil || appendErr != nil || transitionErr != nil {
		t.Fatalf("in-tx helpers: tx %v, append %v, transition %v", err, appendErr, transitionErr)
	}
	byStep, err := store.ListAgentSessionEntries(ctx, SessionEntryFilter{Scope: testSessionScope, StepID: "t1", Descending: true})
	if err != nil || len(byStep) != 2 || byStep[0].ID != "e1" || byStep[0].State != ToolEntryAwaitingChild {
		t.Fatalf("list by step = %v, %v", byStep, err)
	}
	// The public append still checks the binding.
	if _, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: testSessionScope, SessionID: sessionID, RunID: "r1", Lease: testSessionLeasePtr(), Now: testSessionNow,
		Entries: []*SessionEntry{{ID: "n2", Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteTaskPromoted, Text: "x", Data: json.RawMessage(`{}`)}}},
	}); err != ErrSessionMoved {
		t.Fatalf("append after rebind = %v, want ErrSessionMoved", err)
	}
}

// Run-wide lists order by created_at; the stored text form must sort
// temporally even when one time has a fractional second and the next has none.
func TestSQLiteListByRunOrdersByTimeAcrossSessions(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLiteSessionStore(t)
	sessionID := RunAgentSessionID(testSessionScope, "r1")
	putTestSQLiteSessionRun(t, store, "r1", sessionID, AgentRunStatusRunning)
	if _, err := store.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
		Scope: testSessionScope, OwnerRunID: "r1", AgentID: "agent-a", Lease: testSessionLease(), Now: testSessionNow,
	}); err != nil {
		t.Fatal(err)
	}
	times := []time.Time{testSessionNow.Add(500 * time.Millisecond), testSessionNow.Add(time.Second), testSessionNow.Add(1500 * time.Millisecond)}
	for i, at := range times {
		id := "n" + string(rune('a'+i))
		lease := AgentRunLeaseGuard{WorkerID: "w1", Now: testSessionNow}
		if _, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
			Scope: testSessionScope, SessionID: sessionID, RunID: "r1", Lease: &lease, Now: at,
			Entries: []*SessionEntry{{ID: id, Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteTaskPromoted, Text: id, Data: json.RawMessage(`{}`)}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.ListAgentSessionEntries(ctx, SessionEntryFilter{Scope: testSessionScope, RunID: "r1"})
	if err != nil || len(listed) != 3 {
		t.Fatalf("list = %v, %v", listed, err)
	}
	for i, entry := range listed {
		if !entry.CreatedAt.Equal(times[i]) {
			t.Fatalf("entry %d created %v, want %v", i, entry.CreatedAt, times[i])
		}
	}
}
