//go:build integration

package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresMigrationLedgerAllowsOnlyRetiredGap(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "migration_ledger_")
	dsn := pgSchema.DSN
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	check := func(want bool) {
		t.Helper()
		current, version, err := store.postgresSchemaCurrent(ctx, pgSchema.DB)
		if err != nil || current != want || version != currentPostgresSchemaVersion {
			t.Fatalf("current=%v version=%d err=%v; want current=%v", current, version, err, want)
		}
	}
	check(true) // Fresh databases correctly omit retired migrations 20, 22, 34, 47, 49 and 51.
	if applied, err := store.postgresMigrationApplied(ctx, pgSchema.DB, routineEventsMigrationVersion); err != nil || !applied {
		t.Fatalf("routine events migration 80 applied = %v, %v", applied, err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("schema_migrations")+` (version,name) VALUES (34,'historical objective template migration')`); err != nil {
		t.Fatal(err)
	}
	check(true) // Historical ledgers may retain it.
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("schema_migrations")+` (version,name) VALUES (20,'durable event source checkpoints'),(22,'durable event source subscriptions and health')`); err != nil {
		t.Fatal(err)
	}
	check(true) // Retired event source migrations 20 and 22 may remain too.
	if applied, err := store.postgresMigrationApplied(ctx, pgSchema.DB, legacyEventWaitsMigrationVersion); err != nil || !applied {
		t.Fatalf("event wait removal migration 81 applied = %v, %v", applied, err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("schema_migrations")+` (version,name) VALUES (47,'durable run event waits'),(49,'run event retention'),(51,'run event notifications')`); err != nil {
		t.Fatal(err)
	}
	check(true) // Retired event wait migrations 47, 49 and 51 may remain too.
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version=2`); err != nil {
		t.Fatal(err)
	}
	check(false) // The optional historical row must not hide a required gap.
}
