package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPostgresStoreRejectsUnsafeSchemaBeforeConnecting(t *testing.T) {
	_, err := NewPostgresStore(context.Background(), "postgres://unused", WithPostgresSchema(`public; DROP SCHEMA public`))
	if err == nil {
		t.Fatal("unsafe PostgreSQL schema was accepted")
	}
}

func TestPostgresStoreRejectsInvalidPoolBudgetsBeforeConnecting(t *testing.T) {
	tests := []PostgresPoolConfig{
		{MaxOpenConnections: 0, MaxIdleConnections: 0},
		{MaxOpenConnections: 4, MaxIdleConnections: 5},
		{MaxOpenConnections: 4, MaxIdleConnections: 1, ConnectionLifetime: -time.Second},
	}
	for _, pool := range tests {
		_, err := NewPostgresStore(context.Background(), "postgres://unused", WithPostgresPool(pool))
		if err == nil || !strings.Contains(err.Error(), "PostgreSQL") {
			t.Fatalf("pool=%#v error=%v", pool, err)
		}
	}
}

func TestDefaultPostgresPoolConfigIsBounded(t *testing.T) {
	pool := DefaultPostgresPoolConfig()
	if err := pool.validate(); err != nil {
		t.Fatal(err)
	}
	if pool.MaxOpenConnections != 16 || pool.MaxIdleConnections != 4 || pool.ConnectionLifetime <= 0 || pool.ConnectionIdleTime <= 0 {
		t.Fatalf("default pool = %#v", pool)
	}
}

func TestPostgresMigrationRollbackRejectsInvalidTarget(t *testing.T) {
	store := &PostgresStore{}
	if err := store.RollbackPostgresMigrations(context.Background(), currentPostgresSchemaVersion+1); err == nil {
		t.Fatal("invalid migration target was accepted")
	}
}
