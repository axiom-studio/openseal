//go:build integration

package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// SQLite store tests: the store's own migrations on open, so they move to
// the SQLite store package with the store. They keep the integration tag of
// the Postgres file they came from.

// The SQLite store drops the same tables on open and never creates them.
func TestSQLiteStoreDropsEventSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range sqlstore.LegacyEventSourceTables {
		if _, err := db.Exec(`CREATE TABLE ` + table + ` (id TEXT)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO ` + table + ` VALUES ('old')`); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	for range 2 {
		store, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		check := openTestSQLiteDB(t, path)
		for _, table := range sqlstore.LegacyEventSourceTables {
			var count int
			if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("SQLite table %s still exists: %d, %v", table, count, err)
			}
		}
		_ = store.Close()
	}
}

// The SQLite store drops the same tables on open and never creates them.
func TestSQLiteStoreDropsEventWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range sqlstore.LegacyEventWaitTables {
		if _, err := db.Exec(`CREATE TABLE ` + table + ` (id TEXT)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO ` + table + ` VALUES ('old')`); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	for range 2 {
		store, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		check := openTestSQLiteDB(t, path)
		for _, table := range sqlstore.LegacyEventWaitTables {
			var count int
			if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("SQLite table %s still exists: %d, %v", table, count, err)
			}
		}
		_ = store.Close()
	}
}
