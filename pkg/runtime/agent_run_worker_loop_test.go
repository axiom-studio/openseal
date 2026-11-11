package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// workerLoopProducer sleeps once on a timer, then finishes the Run.
type workerLoopProducer struct {
	calls  atomic.Int32
	steps  int // producer calls before the final (each parks on a timer)
	wakeIn time.Duration
}

func (*workerLoopProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (p *workerLoopProducer) Produce(_ context.Context, in StepInput) (StepOutput, error) {
	call := int(p.calls.Add(1))
	if call <= p.steps {
		due := time.Now().Add(p.wakeIn)
		return StepOutput{Park: &LoopPark{
			Status: AgentRunStatusSleeping, Summary: "Waiting for the next step",
			Wake: &WakeCondition{Type: "timer", WakeAt: &due, Reference: "next-step"},
		}}, nil
	}
	return StepOutput{Final: &LoopFinal{Answer: "Work complete", Output: map[string]interface{}{"winner": "agent", "sequence": float64(in.Step.Sequence)}}}, nil
}

func workerLoopStores() []struct {
	name  string
	store func(*testing.T) KernelStore
} {
	return []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "workers.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
}

func waitForRunStatus(t *testing.T, store KernelStore, scope Scope, runID string, status AgentRunStatus) *AgentRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := store.GetAgentRun(context.Background(), scope, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status == status {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("Run did not reach %s: %#v", status, run)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The worker drives each claim through the RunLoop: a parked step exits the
// claim, the timer wake requeues the Run and the next claim finishes it.
func TestAgentRunWorkerDrivesRunLoopAcrossClaims(t *testing.T) {
	for _, test := range workerLoopStores() {
		t.Run(test.name, func(t *testing.T) {
			store := test.store(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scope := Scope{Kind: "local", ID: "test"}
			run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
				Goal: "sleep once, then finish", Source: RunSourceObjective,
			})
			if err != nil {
				t.Fatal(err)
			}
			producer := &workerLoopProducer{steps: 1, wakeIn: 40 * time.Millisecond}
			var resolved atomic.Int32
			resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				resolved.Add(1)
				return &TurnRunnerBinding{
					DefinitionID: "test-agent", DefinitionVersion: "1", ModelProvider: "fake", Model: "deterministic",
					Producer: producer,
				}, nil
			})
			pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
				Scope: scope, AssignedAgentID: "agent", Concurrency: 1,
				PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			pool.Start(ctx)
			defer pool.Stop()
			completed := waitForRunStatus(t, store, scope, run.ID, AgentRunStatusCompleted)
			if completed.LastAppliedTurn != 2 || completed.Output["winner"] != "agent" || completed.Output["sequence"] != float64(2) {
				t.Fatalf("completed Run = %#v", completed)
			}
			if producer.calls.Load() != 2 || resolved.Load() != 2 {
				t.Fatalf("producer calls = %d, resolutions = %d; want one step per claim", producer.calls.Load(), resolved.Load())
			}
			turns, err := store.ListAgentTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 2 || turns[0].Status != AgentTurnStatusCompleted || turns[1].Status != AgentTurnStatusCompleted {
				t.Fatalf("step rows = %#v", turns)
			}
		})
	}
}

// A binding without a step producer cannot run and fails the Run.
func TestAgentRunWorkerFailsRunWithoutStepProducer(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "nothing to run", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DefinitionID: "test-agent", DefinitionVersion: "1"}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{Scope: scope, AssignedAgentID: "agent", LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := pool.scheduler.ClaimNext(ctx, AgentRunClaimRequest{Scope: scope, WorkerID: "worker-1", AssignedAgentID: "agent", LeaseDuration: time.Second})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	pool.executeClaim(ctx, "worker-1", claimed)
	failed, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != AgentRunStatusFailed || failed.Error != "turn runner resolver returned no step producer" {
		t.Fatalf("Run = %s %q", failed.Status, failed.Error)
	}
}
