package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends. runtimetest.NewKernelStore, skillbindingtest.NewStore
// and the openers in the store/sqlite, callback and skillbinding tests are
// its copies for other packages (importing runtimetest here would be a
// cycle); change them together.
func newTestKernelStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
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
func getStoredRun(t testing.TB, store *sqlite.Store, scope Scope, id string) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

// newTestSQLiteStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends. Use it instead of
// newTestKernelStore when the test reads or writes rows no store method
// offers.
func newTestSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, openTestSQLiteDB(t, path)
}

// openTestSQLiteSessionStore opens a SQLite store on a fresh file, closed
// when the test ends.
func openTestSQLiteSessionStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
