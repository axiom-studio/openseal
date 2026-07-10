//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresExecutionStoreConformanceAndReplicaClaims(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	schema := "openseal_test_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	runExecutionStoreConformance(t, primary)

	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	const runCount = 24
	for index := 0; index < runCount; index++ {
		if _, err := primary.CreateRun(ctx, testWorkflow(0), map[string]interface{}{"index": index}); err != nil {
			t.Fatal(err)
		}
	}
	stores := []*PostgresStore{primary, replica}
	claimed := make(chan int, runCount)
	claimErrors := make(chan error, 8)
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			store := stores[worker%len(stores)]
			for {
				run, claimErr := store.ClaimNextRunnable(ctx, "worker-"+string(rune('a'+worker)), time.Minute)
				if claimErr != nil {
					claimErrors <- claimErr
					return
				}
				if run == nil {
					return
				}
				claimed <- run.RunID
			}
		}(worker)
	}
	wait.Wait()
	close(claimed)
	close(claimErrors)
	for claimErr := range claimErrors {
		t.Fatal(claimErr)
	}
	seen := make(map[int]bool)
	for runID := range claimed {
		if seen[runID] {
			t.Fatalf("run %d was claimed more than once", runID)
		}
		seen[runID] = true
	}
	if len(seen) != runCount {
		t.Fatalf("unique claims = %d, want %d", len(seen), runCount)
	}

	retryID, err := primary.CreateRun(ctx, testWorkflow(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.ClaimNextRunnable(ctx, "retry-owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(80 * time.Millisecond)
	if err := primary.ScheduleRetry(ctx, retryID, "retry-owner", due, errors.New("transient")); err != nil {
		t.Fatal(err)
	}
	if early, err := replica.ClaimNextRunnable(ctx, "retry-replica", time.Minute); err != nil || early != nil {
		t.Fatalf("early retry claim = %#v, %v", early, err)
	}
	time.Sleep(time.Until(due) + 20*time.Millisecond)
	recovered, err := replica.ClaimNextRunnable(ctx, "retry-replica", time.Minute)
	if err != nil || recovered == nil || recovered.RunID != retryID || recovered.RetryCount != 1 {
		t.Fatalf("recovered retry = %#v, %v", recovered, err)
	}

	var migrationCount int
	if err := primary.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+primary.table("schema_migrations")+` WHERE version = 1`).Scan(&migrationCount); err != nil || migrationCount != 1 {
		t.Fatalf("migration count = %d, %v", migrationCount, err)
	}
}
