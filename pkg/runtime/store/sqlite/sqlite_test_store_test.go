package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The store's tests open stores and connections of their own. Runtime's
// scenario tests have the same openers in its kernel_store_test.go.

// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends.
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

// openTestSQLiteSessionStore opens a SQLite store on a fresh file, closed
// when the test ends.
func openTestSQLiteSessionStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
