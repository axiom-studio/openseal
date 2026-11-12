//go:build integration

package runtime

import (
	"context"
	"testing"
)

// Store test: rolls back and reapplies the Postgres store's credential lease
// migration, so this file moves to store/postgres (U2.8).

func TestPostgresActionCredentialLeaseMigrationRollsBackAndReapplies(t *testing.T) {
	ctx := context.Background()
	pgSchema := newTestPostgresSchema(t, "openseal_credential_lease_")
	primary := pgSchema.open(t)
	if err := primary.RollbackPostgresMigrations(ctx, 20); err != nil {
		t.Fatal(err)
	}
	// Migration 20 is retired, so 19 is the highest version left.
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != 19 {
		t.Fatalf("rolled-back version = %d, %v", version, err)
	}
	var exists bool
	if err := pgSchema.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='action_credential_lease_redemptions'
	)`, pgSchema.name).Scan(&exists); err != nil || exists {
		t.Fatalf("redemption table after rollback exists=%t, err=%v", exists, err)
	}
	if err := primary.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("reapplied version = %d, %v", version, err)
	}
	if err := pgSchema.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='action_credential_lease_redemptions'
	)`, pgSchema.name).Scan(&exists); err != nil || !exists {
		t.Fatalf("redemption table after reapply exists=%t, err=%v", exists, err)
	}
}
