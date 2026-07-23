package runtime

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestFreshSQLiteStoreDoesNotCreateRetiredRunsTable(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'runs'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("fresh canonical store created the retired numeric runs table")
	}
}

func TestSQLiteStoreOpensExistingDatabaseWithoutReadingRetiredRunsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE runs (run_id INTEGER PRIMARY KEY, payload TEXT); INSERT INTO runs VALUES (1, 'inert')`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var payload string
	if err := store.db.QueryRow(`SELECT payload FROM runs WHERE run_id = 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != "inert" {
		t.Fatalf("retired table was mutated: %q", payload)
	}
}
