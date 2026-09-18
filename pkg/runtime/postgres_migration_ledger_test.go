//go:build integration

package runtime

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresMigrationLedgerAllowsOnlyRetiredGap(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("migration_ledger_"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	check := func(want bool) {
		t.Helper()
		current, version, err := store.postgresSchemaCurrent(ctx, store.db)
		if err != nil || current != want || version != currentPostgresSchemaVersion {
			t.Fatalf("current=%v version=%d err=%v; want current=%v", current, version, err, want)
		}
	}
	check(true) // Fresh databases correctly omit retired migration 34.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("schema_migrations")+` (version,name) VALUES (34,'historical objective template migration')`); err != nil {
		t.Fatal(err)
	}
	check(true) // Historical ledgers may retain it.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM `+store.table("schema_migrations")+` WHERE version=2`); err != nil {
		t.Fatal(err)
	}
	check(false) // The optional historical row must not hide a required gap.
}
