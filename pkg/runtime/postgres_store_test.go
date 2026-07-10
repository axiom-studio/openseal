package runtime

import (
	"context"
	"testing"
)

func TestPostgresStoreRejectsUnsafeSchemaBeforeConnecting(t *testing.T) {
	_, err := NewPostgresStore(context.Background(), "postgres://unused", WithPostgresSchema(`public; DROP SCHEMA public`))
	if err == nil {
		t.Fatal("unsafe PostgreSQL schema was accepted")
	}
}

func TestPostgresMigrationRollbackRejectsInvalidTarget(t *testing.T) {
	store := &PostgresStore{}
	if err := store.RollbackPostgresMigrations(context.Background(), currentPostgresSchemaVersion+1); err == nil {
		t.Fatal("invalid migration target was accepted")
	}
}
