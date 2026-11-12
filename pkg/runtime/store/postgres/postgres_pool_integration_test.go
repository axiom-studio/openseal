//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresPoolBudgetBoundsConcurrentConnectionsAndExposesWaits(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_pool_budget_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn,
		WithPostgresSchema(schema),
		WithPostgresPool(PostgresPoolConfig{
			MaxOpenConnections: 4, MaxIdleConnections: 2,
			ConnectionLifetime: time.Minute, ConnectionIdleTime: time.Minute,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	const callers = 24
	start := make(chan struct{})
	errorsFound := make(chan error, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, queryErr := store.db.ExecContext(ctx, `SELECT pg_sleep(0.1)`)
			errorsFound <- queryErr
		}()
	}
	close(start)
	group.Wait()
	close(errorsFound)
	for queryErr := range errorsFound {
		if queryErr != nil {
			t.Fatal(queryErr)
		}
	}
	stats := store.PoolStats()
	if stats.MaxOpenConnections != 4 || stats.OpenConnections > 4 || stats.InUseConnections > 4 || stats.WaitCount < callers-4 || stats.WaitDuration <= 0 {
		t.Fatalf("bounded pool stats = %#v", stats)
	}
}

func TestPostgresPoolBudgetBoundsTwoOverlappingReplicas(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_pool_replicas_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := PostgresPoolConfig{
		MaxOpenConnections: 3, MaxIdleConnections: 1,
		ConnectionLifetime: time.Minute, ConnectionIdleTime: time.Minute,
	}
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema), WithPostgresPool(pool))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema), WithPostgresPool(pool))
	if err != nil {
		_ = primary.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = replacement.Close()
		_ = primary.Close()
	})

	const callersPerReplica = 15
	start := make(chan struct{})
	errorsFound := make(chan error, callersPerReplica*2)
	var group sync.WaitGroup
	for _, store := range []*Store{primary, replacement} {
		for index := 0; index < callersPerReplica; index++ {
			group.Add(1)
			go func(store *Store) {
				defer group.Done()
				<-start
				_, queryErr := store.db.ExecContext(ctx, `SELECT pg_sleep(0.1)`)
				errorsFound <- queryErr
			}(store)
		}
	}
	close(start)
	group.Wait()
	close(errorsFound)
	for queryErr := range errorsFound {
		if queryErr != nil {
			t.Fatal(queryErr)
		}
	}
	primaryStats, replacementStats := primary.PoolStats(), replacement.PoolStats()
	for name, stats := range map[string]PostgresPoolStats{"primary": primaryStats, "replacement": replacementStats} {
		if stats.MaxOpenConnections != 3 || stats.OpenConnections > 3 || stats.InUseConnections > 3 || stats.IdleConnections > 1 || stats.WaitCount < callersPerReplica-3 || stats.WaitDuration <= 0 {
			t.Fatalf("%s replica pool stats = %#v", name, stats)
		}
	}
	if primaryStats.OpenConnections+replacementStats.OpenConnections > 6 {
		t.Fatalf("overlapping replica connections exceed aggregate budget: primary=%#v replacement=%#v", primaryStats, replacementStats)
	}
}
