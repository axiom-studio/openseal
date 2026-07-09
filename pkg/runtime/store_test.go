package runtime

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	_ "github.com/mattn/go-sqlite3"
)

func TestExecutionStoreConformance(t *testing.T) {
	t.Parallel()
	t.Run("memory", func(t *testing.T) {
		runExecutionStoreConformance(t, NewMemoryStore(100))
	})
	t.Run("sqlite", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "runs.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		runExecutionStoreConformance(t, store)
	})
}

func runExecutionStoreConformance(t *testing.T, store ExecutionStore) {
	t.Helper()
	ctx := context.Background()
	workflow := testWorkflow(0)
	runID, err := store.CreateRun(ctx, workflow, map[string]interface{}{"event": "push"})
	if err != nil {
		t.Fatal(err)
	}

	run, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.Status != RunStatusPending || run.Workflow.Name != workflow.Name {
		t.Fatalf("unexpected pending run: %#v", run)
	}

	claimed, err := store.ClaimNextRunnable(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.RunID != runID || claimed.LeaseOwner != "worker-a" {
		t.Fatalf("unexpected claim: %#v", claimed)
	}
	second, err := store.ClaimNextRunnable(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatalf("run was claimed twice: %#v", second)
	}
	if err := store.RenewLease(ctx, runID, "worker-b", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign renewal error = %v, want ErrLeaseLost", err)
	}

	nodeResult := &executor.NodeResult{NodeId: "start", Status: "completed"}
	if err := store.UpdateNodeResult(ctx, runID, "worker-a", "start", nodeResult); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(ctx, runID, "worker-a", RunStatusCompleted, nil); err != nil {
		t.Fatal(err)
	}
	completed, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != RunStatusCompleted || completed.CompletedAt == nil || completed.LeaseOwner != "" {
		t.Fatalf("unexpected completed run: %#v", completed)
	}
}

func TestExecutionStoreExpiredLeaseIsReclaimedOnce(t *testing.T) {
	stores := map[string]func(t *testing.T) ExecutionStore{
		"memory": func(t *testing.T) ExecutionStore { return NewMemoryStore(10) },
		"sqlite": func(t *testing.T) ExecutionStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			return store
		},
	}
	for name, factory := range stores {
		t.Run(name, func(t *testing.T) {
			store := factory(t)
			ctx := context.Background()
			runID, err := store.CreateRun(ctx, testWorkflow(0), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimNextRunnable(ctx, "dead-worker", 10*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)

			var wg sync.WaitGroup
			claims := make(chan *RunRecord, 2)
			for _, worker := range []string{"recovery-a", "recovery-b"} {
				wg.Add(1)
				go func(worker string) {
					defer wg.Done()
					run, claimErr := store.ClaimNextRunnable(ctx, worker, time.Minute)
					if claimErr != nil {
						t.Errorf("claim: %v", claimErr)
						return
					}
					claims <- run
				}(worker)
			}
			wg.Wait()
			close(claims)
			count := 0
			for claim := range claims {
				if claim != nil {
					count++
					if claim.RunID != runID {
						t.Fatalf("claimed run %d, want %d", claim.RunID, runID)
					}
				}
			}
			if count != 1 {
				t.Fatalf("successful recovery claims = %d, want 1", count)
			}
		})
	}
}

func TestSQLiteRetrySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID, err := store.CreateRun(ctx, testWorkflow(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextRunnable(ctx, "worker-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(40 * time.Millisecond)
	if err := store.ScheduleRetry(ctx, runID, "worker-a", due, errors.New("transient")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claim, err := reopened.ClaimNextRunnable(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim != nil {
		t.Fatalf("retry claimed before due time: %#v", claim)
	}
	time.Sleep(time.Until(due) + 10*time.Millisecond)
	claim, err = reopened.ClaimNextRunnable(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.RunID != runID || claim.RetryCount != 1 {
		t.Fatalf("unexpected recovered retry: %#v", claim)
	}
}

func TestSQLiteStoreUpgradesLegacyRunSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE runs (
			run_id INTEGER PRIMARY KEY AUTOINCREMENT,
			workflow_name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			node_results TEXT DEFAULT '{}',
			started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME,
			error TEXT,
			retry_count INTEGER DEFAULT 0
		);
		INSERT INTO runs (workflow_name, status) VALUES ('legacy', 'completed');
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.GetRun(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.WorkflowName != "legacy" || run.CreatedAt.IsZero() || run.AvailableAt.IsZero() {
		t.Fatalf("legacy run was not upgraded: %#v", run)
	}
}

func testWorkflow(delay time.Duration) WorkflowEntry {
	config := map[string]interface{}{"milliseconds": float64(delay.Milliseconds())}
	return WorkflowEntry{
		Name: "test-workflow",
		Nodes: []*executor.NodeDefinition{{
			Id: "start", Name: "start", Type: executor.StepTypeDelay, Config: config,
		}},
		StartNodeID: "start",
	}
}
