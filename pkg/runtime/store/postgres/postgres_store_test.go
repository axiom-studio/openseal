package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPostgresStoreRejectsUnsafeSchemaBeforeConnecting(t *testing.T) {
	_, err := New(context.Background(), "postgres://unused", WithSchema(`public; DROP SCHEMA public`))
	if err == nil {
		t.Fatal("unsafe PostgreSQL schema was accepted")
	}
}

func TestPostgresStoreRejectsInvalidPoolBudgetsBeforeConnecting(t *testing.T) {
	tests := []PoolConfig{
		{MaxOpenConnections: 0, MaxIdleConnections: 0},
		{MaxOpenConnections: 4, MaxIdleConnections: 5},
		{MaxOpenConnections: 4, MaxIdleConnections: 1, ConnectionLifetime: -time.Second},
	}
	for _, pool := range tests {
		_, err := New(context.Background(), "postgres://unused", WithPool(pool))
		if err == nil || !strings.Contains(err.Error(), "PostgreSQL") {
			t.Fatalf("pool=%#v error=%v", pool, err)
		}
	}
}

func TestDefaultPostgresPoolConfigIsBounded(t *testing.T) {
	pool := DefaultPoolConfig()
	if err := pool.validate(); err != nil {
		t.Fatal(err)
	}
	if pool.MaxOpenConnections != 16 || pool.MaxIdleConnections != 4 || pool.ConnectionLifetime <= 0 || pool.ConnectionIdleTime <= 0 {
		t.Fatalf("default pool = %#v", pool)
	}
}

func TestPostgresStoreRejectsInvalidMigrationLockBudgetsBeforeConnecting(t *testing.T) {
	tests := [][2]time.Duration{
		{0, time.Millisecond},
		{11 * time.Minute, time.Millisecond},
		{time.Second, 0},
		{time.Second, 2 * time.Second},
	}
	for _, budget := range tests {
		_, err := New(context.Background(), "postgres://unused", WithMigrationLock(budget[0], budget[1]))
		if err == nil || !strings.Contains(err.Error(), "migration lock") {
			t.Fatalf("budget=%v error=%v", budget, err)
		}
	}
}

func TestPostgresMigrationRollbackRejectsInvalidTarget(t *testing.T) {
	store := &Store{}
	if err := store.RollbackPostgresMigrations(context.Background(), currentPostgresSchemaVersion+1); err == nil {
		t.Fatal("invalid migration target was accepted")
	}
}
