//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Store test: rolls back and reapplies the Postgres store's credential lease
// migration, so this file moves to store/postgres (U2.8).

func TestPostgresActionCredentialLeaseMigrationRollsBackAndReapplies(t *testing.T) {
	ctx := context.Background()
	pgSchema := storetest.NewPostgresSchema(t, "openseal_credential_lease_")
	primary := openPostgresStoreOn(t, pgSchema)
	if err := primary.RollbackPostgresMigrations(ctx, 20); err != nil {
		t.Fatal(err)
	}
	// Migration 20 is retired, so 19 is the highest version left.
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != 19 {
		t.Fatalf("rolled-back version = %d, %v", version, err)
	}
	var exists bool
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='action_credential_lease_redemptions'
	)`, pgSchema.Name).Scan(&exists); err != nil || exists {
		t.Fatalf("redemption table after rollback exists=%t, err=%v", exists, err)
	}
	if err := primary.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("reapplied version = %d, %v", version, err)
	}
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name='action_credential_lease_redemptions'
	)`, pgSchema.Name).Scan(&exists); err != nil || !exists {
		t.Fatalf("redemption table after reapply exists=%t, err=%v", exists, err)
	}
}
