package runworker

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// workerLoopProducer sleeps once on a timer, then finishes the Run.
type workerLoopProducer struct {
	calls  atomic.Int32
	steps  int // producer calls before the final (each parks on a timer)
	wakeIn time.Duration
}

func (*workerLoopProducer) Kind() models.StepProducerKind { return models.StepProducerDeterministic }

func (p *workerLoopProducer) Produce(_ context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	call := int(p.calls.Add(1))
	if call <= p.steps {
		due := time.Now().Add(p.wakeIn)
		return agentrun.StepOutput{Park: &agentrun.LoopPark{
			Status: models.AgentRunStatusSleeping, Summary: "Waiting for the next step",
			Wake: &models.WakeCondition{Type: "timer", WakeAt: &due, Reference: "next-step"},
		}}, nil
	}
	return agentrun.StepOutput{Final: &agentrun.LoopFinal{Answer: "Work complete", Output: map[string]interface{}{"winner": "agent", "sequence": float64(in.Step.Sequence)}}}, nil
}

func workerLoopStores() []struct {
	name  string
	store func(*testing.T) models.KernelStore
} {
	return []struct {
		name  string
		store func(*testing.T) models.KernelStore
	}{
		{name: "sqlite", store: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "workers.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
}

func waitForRunStatus(t *testing.T, store models.KernelStore, scope models.Scope, runID string, status models.AgentRunStatus) *models.AgentRun {
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
			scope := models.Scope{Kind: "local", ID: "test"}
			run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
				Goal: "sleep once, then finish", Source: models.RunSourceObjective,
			})
			if err != nil {
				t.Fatal(err)
			}
			producer := &workerLoopProducer{steps: 1, wakeIn: 40 * time.Millisecond}
			var resolved atomic.Int32
			resolver := conversation.TurnRunnerResolverFunc(func(context.Context, *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
				resolved.Add(1)
				return &agentrun.TurnRunnerBinding{
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
			completed := waitForRunStatus(t, store, scope, run.ID, models.AgentRunStatusCompleted)
			if completed.LastAppliedTurn != 2 || completed.Output["winner"] != "agent" || completed.Output["sequence"] != float64(2) {
				t.Fatalf("completed Run = %#v", completed)
			}
			if producer.calls.Load() != 2 || resolved.Load() != 2 {
				t.Fatalf("producer calls = %d, resolutions = %d; want one step per claim", producer.calls.Load(), resolved.Load())
			}
			turns, err := store.ListAgentTurns(ctx, models.AgentTurnFilter{Scope: scope, RunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 2 || turns[0].Status != models.AgentTurnStatusCompleted || turns[1].Status != models.AgentTurnStatusCompleted {
				t.Fatalf("step rows = %#v", turns)
			}
		})
	}
}

// A binding without a step producer cannot run and fails the Run.
func TestAgentRunWorkerFailsRunWithoutStepProducer(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "test"}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "nothing to run", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := conversation.TurnRunnerResolverFunc(func(context.Context, *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return &agentrun.TurnRunnerBinding{DefinitionID: "test-agent", DefinitionVersion: "1"}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{Scope: scope, AssignedAgentID: "agent", LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := pool.scheduler.ClaimNext(ctx, agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker-1", AssignedAgentID: "agent", LeaseDuration: time.Second})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	pool.executeClaim(ctx, "worker-1", claimed)
	failed, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != models.AgentRunStatusFailed || failed.Error != "turn runner resolver returned no step producer" {
		t.Fatalf("Run = %s %q", failed.Status, failed.Error)
	}
}
