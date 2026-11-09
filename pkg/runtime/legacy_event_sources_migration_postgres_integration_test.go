//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// Migration 79 drops the event source tables. A fresh database never has
// them; a database at 78 loses them with their rows, and the retired ledger
// rows 20 and 22 it keeps do not make the schema look incomplete.
func TestPostgresMigration79DropsEventSources(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	schema := "event_sources_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	assertNoEventSourceTables(t, ctx, store)
	assertSchemaCurrent(t, ctx, store)

	// Put the database back at 78 as an old release left it.
	for _, statement := range []string{
		`CREATE TABLE ` + store.table("event_source_checkpoints") + ` (scope_kind TEXT, scope_id TEXT, source TEXT, payload JSONB)`,
		`CREATE TABLE ` + store.table("event_source_subscriptions") + ` (scope_kind TEXT, scope_id TEXT, id TEXT, PRIMARY KEY (scope_kind, scope_id, id))`,
		`CREATE TABLE ` + store.table("event_source_health") + ` (scope_kind TEXT, scope_id TEXT, subscription_id TEXT,
			FOREIGN KEY (scope_kind, scope_id, subscription_id) REFERENCES ` + store.table("event_source_subscriptions") + ` (scope_kind, scope_id, id))`,
		`INSERT INTO ` + store.table("event_source_checkpoints") + ` VALUES ('tenant', 'one', 'forum', '{}')`,
		`INSERT INTO ` + store.table("event_source_subscriptions") + ` VALUES ('tenant', 'one', 'watch')`,
		`INSERT INTO ` + store.table("event_source_health") + ` VALUES ('tenant', 'one', 'watch')`,
		`INSERT INTO ` + store.table("schema_migrations") + ` (version, name) VALUES (20, 'durable event source checkpoints'), (22, 'durable event source subscriptions and health')`,
		`DELETE FROM ` + store.table("schema_migrations") + ` WHERE version = 79`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if current, _, err := store.postgresSchemaCurrent(ctx, store.db); err != nil || current {
		t.Fatalf("a database at 78 looks current: %v, %v", current, err)
	}
	_ = store.Close()
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertNoEventSourceTables(t, ctx, reopened)
	assertSchemaCurrent(t, ctx, reopened)

	// Rolling back 79 restores nothing, and migrating again is a no-op.
	if err := reopened.RollbackPostgresMigrations(ctx, 78); err != nil {
		t.Fatal(err)
	}
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != 78 {
		t.Fatalf("version after rollback = %d, %v", version, err)
	}
	assertNoEventSourceTables(t, ctx, reopened)
	_ = reopened.Close()
	again, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	assertSchemaCurrent(t, ctx, again)
}

// The SQLite store drops the same tables on open and never creates them.
func TestSQLiteStoreDropsEventSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range legacyEventSourceTables {
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
		for _, table := range legacyEventSourceTables {
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("SQLite table %s still exists: %d, %v", table, count, err)
			}
		}
		_ = store.Close()
	}
}

func assertNoEventSourceTables(t *testing.T, ctx context.Context, store *PostgresStore) {
	t.Helper()
	for _, table := range legacyEventSourceTables {
		var relation sql.NullString
		if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, store.schema+"."+table).Scan(&relation); err != nil || relation.Valid {
			t.Fatalf("table %s still exists: %v, %v", table, relation.String, err)
		}
	}
}

func assertSchemaCurrent(t *testing.T, ctx context.Context, store *PostgresStore) {
	t.Helper()
	current, version, err := store.postgresSchemaCurrent(ctx, store.db)
	if err != nil || !current || version != currentPostgresSchemaVersion {
		t.Fatalf("schema current = %v at %d, %v", current, version, err)
	}
}
