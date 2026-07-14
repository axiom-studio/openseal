package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	"go.uber.org/zap"
)

type concurrencyExecutor struct {
	mu      sync.Mutex
	active  int
	maximum int
	delay   time.Duration
}

func (e *concurrencyExecutor) Type() string { return "concurrency-test" }

func (e *concurrencyExecutor) Execute(ctx context.Context, _ *executor.StepDefinition, _ executor.TemplateResolver) (*executor.StepResult, error) {
	e.mu.Lock()
	e.active++
	if e.active > e.maximum {
		e.maximum = e.active
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active--
		e.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(e.delay):
		return &executor.StepResult{Output: map[string]interface{}{"ok": true}}, nil
	}
}

func (e *concurrencyExecutor) maxActive() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maximum
}

func TestWorkerPoolHonorsConfiguredConcurrency(t *testing.T) {
	store := NewMemoryStore(100)
	exec := &concurrencyExecutor{delay: 30 * time.Millisecond}
	pool, scheduler := testPool(store, exec, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()

	workflow := WorkflowEntry{
		Name:        "concurrency",
		Nodes:       []*executor.NodeDefinition{{Id: "start", Name: "start", Type: exec.Type(), Config: map[string]interface{}{}}},
		StartNodeID: "start",
	}
	for i := 0; i < 6; i++ {
		if _, err := scheduler.Schedule(ctx, workflow, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitForRuns(t, store, 6, RunStatusCompleted)
	if got := exec.maxActive(); got != 2 {
		t.Fatalf("maximum concurrent executions = %d, want 2", got)
	}
}

func TestWorkerPoolHonorsSharedProcessLimiter(t *testing.T) {
	store := NewMemoryStore(100)
	exec := &concurrencyExecutor{delay: 10 * time.Millisecond}
	pool, scheduler := testPool(store, exec, 8)
	limiter, err := NewWorkerLimiter(2)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetWorkerLimiter(limiter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	workflow := WorkflowEntry{
		Name: "globally-bounded", Nodes: []*executor.NodeDefinition{{Id: "start", Name: "start", Type: exec.Type(), Config: map[string]interface{}{}}},
		StartNodeID: "start",
	}
	for index := 0; index < 12; index++ {
		if _, err := scheduler.Schedule(ctx, workflow, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitForRuns(t, store, 12, RunStatusCompleted)
	stats := limiter.Stats()
	if exec.maxActive() != 2 || stats.MaximumInUse != 2 || stats.WaitCount == 0 {
		t.Fatalf("maximum executions=%d limiter=%#v", exec.maxActive(), stats)
	}
}

func TestWorkerPoolRecoversPersistedSQLiteRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	exec := &concurrencyExecutor{delay: time.Millisecond}
	workflow := WorkflowEntry{
		Name:        "recovery",
		Nodes:       []*executor.NodeDefinition{{Id: "start", Name: "start", Type: exec.Type(), Config: map[string]interface{}{}}},
		StartNodeID: "start",
	}
	runID, err := store.CreateRun(context.Background(), workflow, map[string]interface{}{"source": "before-restart"})
	if err != nil {
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
	pool, _ := testPool(reopened, exec, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()

	waitForRuns(t, reopened, 1, RunStatusCompleted)
	run, err := reopened.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.TriggerData["source"] != "before-restart" {
		t.Fatalf("recovered run lost its snapshot: %#v", run)
	}
}

func testPool(store ExecutionStore, stepExecutor executor.StepExecutor, concurrency int) (*WorkerPool, *Scheduler) {
	registry := executor.NewEmptyRegistry()
	registry.Register(stepExecutor)
	logger := zap.NewNop().Sugar()
	pool := NewWorkerPool(executor.NewPipelineExecutor(registry, logger), store, logger, concurrency, DefaultRetryPolicy())
	pool.pollInterval = 5 * time.Millisecond
	return pool, NewScheduler(pool, store)
}

func waitForRuns(t *testing.T, store ExecutionStore, count int, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := store.ListRuns(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		matched := 0
		for _, run := range runs {
			if run.Status == status {
				matched++
			}
		}
		if matched == count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d runs in state %s", count, status)
}
