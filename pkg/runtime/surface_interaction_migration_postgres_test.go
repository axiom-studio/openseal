package runtime

import (
	"context"
	"testing"
	"time"
)

func newSurfaceInteractionPostgresStore(t *testing.T) (context.Context, *PostgresStore) {
	t.Helper()
	pgSchema := newTestPostgresSchema(t, "openseal_surface_interactions_")
	dsn := pgSchema.dsn
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return ctx, store
}

func TestPostgresSurfaceInteractionMigrationRollsBackAndReapplies(t *testing.T) {
	ctx, store := newSurfaceInteractionPostgresStore(t)
	exists := func() bool {
		var exists bool
		if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, store.schema+".surface_interactions").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion || !exists() {
		t.Fatalf("schema version = %d, %v; table %t", version, err, exists())
	}
	if err := store.RollbackPostgresMigrations(ctx, surfaceInteractionMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != surfaceInteractionMigrationVersion-1 || exists() {
		t.Fatalf("after rollback: version %d, %v; table %t", version, err, exists())
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion || !exists() {
		t.Fatalf("after reapply: version %d, %v; table %t", version, err, exists())
	}
}
