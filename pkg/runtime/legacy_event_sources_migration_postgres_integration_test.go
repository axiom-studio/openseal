//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Postgres store test: it checks the store's own migration ledger, so it moves
// to the Postgres store package with the store.

// Migration 79 drops the event source tables. A fresh database never has
// them; a database at 78 loses them with their rows, and the retired ledger
// rows 20 and 22 it keeps do not make the schema look incomplete.
func TestPostgresMigration79DropsEventSources(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "event_sources_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	assertNoEventSourceTables(t, ctx, store)
	assertSchemaCurrent(t, ctx, store)

	// Put the database back at 78 as an old release left it.
	for _, statement := range []string{
		`CREATE TABLE ` + pgSchema.Table("event_source_checkpoints") + ` (scope_kind TEXT, scope_id TEXT, source TEXT, payload JSONB)`,
		`CREATE TABLE ` + pgSchema.Table("event_source_subscriptions") + ` (scope_kind TEXT, scope_id TEXT, id TEXT, PRIMARY KEY (scope_kind, scope_id, id))`,
		`CREATE TABLE ` + pgSchema.Table("event_source_health") + ` (scope_kind TEXT, scope_id TEXT, subscription_id TEXT,
			FOREIGN KEY (scope_kind, scope_id, subscription_id) REFERENCES ` + pgSchema.Table("event_source_subscriptions") + ` (scope_kind, scope_id, id))`,
		`INSERT INTO ` + pgSchema.Table("event_source_checkpoints") + ` VALUES ('tenant', 'one', 'forum', '{}')`,
		`INSERT INTO ` + pgSchema.Table("event_source_subscriptions") + ` VALUES ('tenant', 'one', 'watch')`,
		`INSERT INTO ` + pgSchema.Table("event_source_health") + ` VALUES ('tenant', 'one', 'watch')`,
		`INSERT INTO ` + pgSchema.Table("schema_migrations") + ` (version, name) VALUES (20, 'durable event source checkpoints'), (22, 'durable event source subscriptions and health')`,
		`DELETE FROM ` + pgSchema.Table("schema_migrations") + ` WHERE version = 79`,
	} {
		if _, err := pgSchema.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if current, _, err := store.postgresSchemaCurrent(ctx, pgSchema.DB); err != nil || current {
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

func assertNoEventSourceTables(t *testing.T, ctx context.Context, store *PostgresStore) {
	t.Helper()
	for _, table := range sqlstore.LegacyEventSourceTables {
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
