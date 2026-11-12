package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// The store's own session writes: the log migration replay, the in-tx
// helpers a caller's transaction fences, and run-wide ordering. The
// conformance suite runs in agent_session_sqlite_test.go.

// sqliteSessionScope and sqliteSessionNow are the scope and clock of the
// sessions these tests write.
var (
	sqliteSessionScope = models.Scope{Kind: "org", ID: "o1"}
	sqliteSessionNow   = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
)

// sqliteSessionLease is worker w1's lease guard at sqliteSessionNow.
func sqliteSessionLease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: "w1", Now: sqliteSessionNow}
}

func sqliteSessionLeasePtr() *models.AgentRunLeaseGuard {
	lease := sqliteSessionLease()
	return &lease
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

func putTestSQLiteSessionRun(t *testing.T, store *SQLiteStore, runID, sessionID string, status models.AgentRunStatus) {
	t.Helper()
	ctx := context.Background()
	expires := sqliteSessionNow.Add(time.Minute)
	run := &models.AgentRun{
		ID: runID, Scope: sqliteSessionScope, Status: status, Revision: 1, SessionID: sessionID,
		LeaseOwner: "w1", LeaseExpiresAt: &expires, CreatedAt: sqliteSessionNow, UpdatedAt: sqliteSessionNow,
	}
	err := store.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		current, err := getSQLiteAgentRun(ctx, conn, sqliteSessionScope, runID)
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
	sessionID := models.ConversationAgentSessionID(sqliteSessionScope, models.ConversationRunLane("c1", "", "agent-a"))
	putTestSQLiteSessionRun(t, store, "r1", sessionID, models.AgentRunStatusRunning)
	opened, err := store.OpenConversationAgentSession(ctx, models.OpenConversationAgentSessionRequest{
		Scope: sqliteSessionScope, ConversationID: "c1", AgentID: "agent-a", RunID: "r1", Lease: sqliteSessionLease(), Now: sqliteSessionNow,
	})
	if err != nil || !opened.Created {
		t.Fatalf("open = %+v, %v", opened, err)
	}
	if _, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: sqliteSessionScope, SessionID: sessionID, RunID: "r1", Lease: sqliteSessionLeasePtr(), Now: sqliteSessionNow,
		Entries: []*models.SessionEntry{storetest.AssistantSessionEntry("a1", "t1", "c1"), storetest.ToolSessionEntry("e1", "t1", "c1", models.ToolEntryPending)},
	}); err != nil {
		t.Fatal(err)
	}
	// Promotion-style note from a Run that is now bound elsewhere, and a
	// transition under another worker: the caller's transaction is the fence.
	putTestSQLiteSessionRun(t, store, "r1", "child-session", models.AgentRunStatusRunning)
	note := &models.SessionEntry{ID: "n1", Type: models.SessionEntryNote, SourceKey: models.TaskPromotedSourceKey("r1"), Note: &models.NoteEntry{Kind: models.NoteTaskPromoted, Text: "moved", Data: json.RawMessage(`{}`)}}
	var appendErr, transitionErr error
	err = store.withSQLiteSessionWrite(ctx, func(conn *sql.Conn) error {
		_, appendErr = store.appendAgentSessionEntriesTx(ctx, conn, models.AppendAgentSessionEntriesRequest{
			Scope: sqliteSessionScope, SessionID: sessionID, RunID: "r1", Entries: []*models.SessionEntry{note},
		})
		_, transitionErr = store.transitionToolEntryTx(ctx, conn, models.ToolEntryTransitionRequest{
			Scope: sqliteSessionScope, EntryID: "e1", RunID: "r1", Lease: models.AgentRunLeaseGuard{WorkerID: "w-other"},
			From: models.ToolEntryPending, To: models.ToolEntryAwaitingChild, Wait: &models.ToolEntryWait{ChildRunIDs: []string{"child"}},
		})
		return nil
	})
	if err != nil || appendErr != nil || transitionErr != nil {
		t.Fatalf("in-tx helpers: tx %v, append %v, transition %v", err, appendErr, transitionErr)
	}
	byStep, err := store.ListAgentSessionEntries(ctx, models.SessionEntryFilter{Scope: sqliteSessionScope, StepID: "t1", Descending: true})
	if err != nil || len(byStep) != 2 || byStep[0].ID != "e1" || byStep[0].State != models.ToolEntryAwaitingChild {
		t.Fatalf("list by step = %v, %v", byStep, err)
	}
	// The public append still checks the binding.
	if _, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: sqliteSessionScope, SessionID: sessionID, RunID: "r1", Lease: sqliteSessionLeasePtr(), Now: sqliteSessionNow,
		Entries: []*models.SessionEntry{{ID: "n2", Type: models.SessionEntryNote, Note: &models.NoteEntry{Kind: models.NoteTaskPromoted, Text: "x", Data: json.RawMessage(`{}`)}}},
	}); err != models.ErrSessionMoved {
		t.Fatalf("append after rebind = %v, want ErrSessionMoved", err)
	}
}

// Run-wide lists order by created_at; the stored text form must sort
// temporally even when one time has a fractional second and the next has none.
func TestSQLiteListByRunOrdersByTimeAcrossSessions(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLiteSessionStore(t)
	sessionID := models.RunAgentSessionID(sqliteSessionScope, "r1")
	putTestSQLiteSessionRun(t, store, "r1", sessionID, models.AgentRunStatusRunning)
	if _, err := store.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{
		Scope: sqliteSessionScope, OwnerRunID: "r1", AgentID: "agent-a", Lease: sqliteSessionLease(), Now: sqliteSessionNow,
	}); err != nil {
		t.Fatal(err)
	}
	times := []time.Time{sqliteSessionNow.Add(500 * time.Millisecond), sqliteSessionNow.Add(time.Second), sqliteSessionNow.Add(1500 * time.Millisecond)}
	for i, at := range times {
		id := "n" + string(rune('a'+i))
		lease := models.AgentRunLeaseGuard{WorkerID: "w1", Now: sqliteSessionNow}
		if _, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
			Scope: sqliteSessionScope, SessionID: sessionID, RunID: "r1", Lease: &lease, Now: at,
			Entries: []*models.SessionEntry{{ID: id, Type: models.SessionEntryNote, Note: &models.NoteEntry{Kind: models.NoteTaskPromoted, Text: id, Data: json.RawMessage(`{}`)}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.ListAgentSessionEntries(ctx, models.SessionEntryFilter{Scope: sqliteSessionScope, RunID: "r1"})
	if err != nil || len(listed) != 3 {
		t.Fatalf("list = %v, %v", listed, err)
	}
	for i, entry := range listed {
		if !entry.CreatedAt.Equal(times[i]) {
			t.Fatalf("entry %d created %v, want %v", i, entry.CreatedAt, times[i])
		}
	}
}
