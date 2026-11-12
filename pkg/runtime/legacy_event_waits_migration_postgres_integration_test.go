//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// Migration 81 drops the event wait tables. A fresh database never has them;
// a database at 80 loses them with their rows, and the retired ledger rows
// 47, 49 and 51 it keeps do not make the schema look incomplete.
func TestPostgresMigration81DropsEventWaits(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "event_waits_")
	dsn := pgSchema.dsn
	schema := pgSchema.name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.quoted()+` CASCADE`)
		_ = store.Close()
	})
	assertNoEventWaitTables(t, ctx, store)
	assertSchemaCurrent(t, ctx, store)

	// Put the database back at 80 as an old release left it.
	for _, statement := range []string{
		`CREATE TABLE ` + pgSchema.table("run_event_inbox") + ` (scope_kind TEXT, scope_id TEXT, source TEXT, event_id TEXT, payload JSONB,
			PRIMARY KEY (scope_kind, scope_id, source, event_id))`,
		`CREATE TABLE ` + pgSchema.table("run_event_waits") + ` (scope_kind TEXT, scope_id TEXT, run_id TEXT, wait_key TEXT, status TEXT, payload JSONB,
			PRIMARY KEY (scope_kind, scope_id, run_id, wait_key))`,
		`CREATE TABLE ` + pgSchema.table("run_event_wait_scopes") + ` (scope_kind TEXT, scope_id TEXT, PRIMARY KEY (scope_kind, scope_id))`,
		`CREATE TABLE ` + pgSchema.table("run_event_consumptions") + ` (scope_kind TEXT, scope_id TEXT, source TEXT, event_id TEXT, run_id TEXT,
			FOREIGN KEY (scope_kind, scope_id, source, event_id) REFERENCES ` + pgSchema.table("run_event_inbox") + ` (scope_kind, scope_id, source, event_id))`,
		`CREATE TABLE ` + pgSchema.table("run_event_notifications") + ` (scope_kind TEXT, scope_id TEXT, source TEXT, event_id TEXT,
			FOREIGN KEY (scope_kind, scope_id, source, event_id) REFERENCES ` + pgSchema.table("run_event_inbox") + ` (scope_kind, scope_id, source, event_id))`,
		`CREATE TABLE ` + pgSchema.table("run_event_retention_cursor") + ` (id INTEGER PRIMARY KEY, received_at TIMESTAMPTZ)`,
		`INSERT INTO ` + pgSchema.table("run_event_inbox") + ` VALUES ('tenant', 'one', 'binding:a', 'e1', '{}')`,
		`INSERT INTO ` + pgSchema.table("run_event_waits") + ` VALUES ('tenant', 'one', 'run-1', 'reply', 'pending', '{}')`,
		`INSERT INTO ` + pgSchema.table("run_event_wait_scopes") + ` VALUES ('tenant', 'one')`,
		`INSERT INTO ` + pgSchema.table("run_event_consumptions") + ` VALUES ('tenant', 'one', 'binding:a', 'e1', 'run-1')`,
		`INSERT INTO ` + pgSchema.table("run_event_notifications") + ` VALUES ('tenant', 'one', 'binding:a', 'e1')`,
		`INSERT INTO ` + pgSchema.table("run_event_retention_cursor") + ` VALUES (1, now())`,
		`INSERT INTO ` + pgSchema.table("schema_migrations") + ` (version, name) VALUES (47, 'durable run event waits'), (49, 'run event retention'), (51, 'run event notifications')`,
		`DELETE FROM ` + pgSchema.table("schema_migrations") + ` WHERE version = 81`,
	} {
		if _, err := pgSchema.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if current, _, err := store.postgresSchemaCurrent(ctx, pgSchema.db); err != nil || current {
		t.Fatalf("a database at 80 looks current: %v, %v", current, err)
	}
	_ = store.Close()
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertNoEventWaitTables(t, ctx, reopened)
	assertSchemaCurrent(t, ctx, reopened)

	// Rolling back 81 restores nothing, and migrating again is a no-op.
	if err := reopened.RollbackPostgresMigrations(ctx, 80); err != nil {
		t.Fatal(err)
	}
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != 80 {
		t.Fatalf("version after rollback = %d, %v", version, err)
	}
	assertNoEventWaitTables(t, ctx, reopened)
	_ = reopened.Close()
	again, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	assertSchemaCurrent(t, ctx, again)
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
		for _, table := range sqlstore.LegacyEventWaitTables {
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("SQLite table %s still exists: %d, %v", table, count, err)
			}
		}
		_ = store.Close()
	}
}

func assertNoEventWaitTables(t *testing.T, ctx context.Context, store *PostgresStore) {
	t.Helper()
	for _, table := range sqlstore.LegacyEventWaitTables {
		var relation sql.NullString
		if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, store.schema+"."+table).Scan(&relation); err != nil || relation.Valid {
			t.Fatalf("table %s still exists: %v, %v", table, relation.String, err)
		}
	}
}
