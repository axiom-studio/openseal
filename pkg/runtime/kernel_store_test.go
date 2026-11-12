package runtime

import (
	"context"
	"database/sql"
	"testing"
)

// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends. runtimetest.NewKernelStore is its copy for other
// packages (importing runtimetest here would be a cycle); change both.
func newTestKernelStore(t testing.TB) *SQLiteStore {
	t.Helper()
	store, err := NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// openTestSQLiteDB opens the test's own connection to the SQLite file at
// path, closed when the test ends.
func openTestSQLiteDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// getStoredRun reads a Run that the test knows exists.
func getStoredRun(t testing.TB, store *SQLiteStore, scope Scope, id string) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}
