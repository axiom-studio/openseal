package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentRunWorkerPoolAdvancesSleepsAndResumes(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore(50) }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "workers.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	for _, test := range tests {
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
			var calls atomic.Int32
			resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{
					DefinitionID: "test-agent", DefinitionVersion: "1", ModelProvider: "fake", Model: "deterministic",
					Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
						call := calls.Add(1)
						if call == 1 {
							due := time.Now().Add(40 * time.Millisecond)
							return &TurnOutcome{
								NextRunStatus: AgentRunStatusSleeping, OutputSummary: "Waiting for the next turn",
								WakeCondition:          &WakeCondition{Type: "timer", WakeAt: &due, Reference: "next-turn"},
								ContinuationCheckpoint: map[string]interface{}{"step": float64(1)},
							}, nil
						}
						if input.Run.LastAppliedTurn != 1 || input.Run.Checkpoint["step"] != float64(1) {
							return nil, fmt.Errorf("second turn did not resume from its checkpoint: %#v", input.Run)
						}
						return &TurnOutcome{
							NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Work complete",
							RunOutput: map[string]interface{}{"winner": "agent"},
						}, nil
					}),
				}, nil
			})
			pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
				Scope: scope, AssignedAgentID: "agent", Concurrency: 1, MaxTurnsPerClaim: 1,
				PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			pool.Start(ctx)
			defer pool.Stop()
			deadline := time.Now().Add(3 * time.Second)
			var completed *AgentRun
			for time.Now().Before(deadline) {
				completed, err = NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if completed.Status == AgentRunStatusCompleted {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if completed == nil || completed.Status != AgentRunStatusCompleted || completed.LastAppliedTurn != 2 ||
				completed.Output["winner"] != "agent" || calls.Load() != 2 {
				t.Fatalf("autonomous run did not complete across sleep: run=%#v calls=%d", completed, calls.Load())
			}
			turns, err := NewAgentTurnService(store, store).ListTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 2 || turns[0].Sequence != 1 || turns[1].Sequence != 2 {
				t.Fatalf("unexpected durable turn history: %#v", turns)
			}
		})
	}
}

func TestAgentRunWorkerPoolYieldsBetweenTurnSlices(t *testing.T) {
	store := NewMemoryStore(20)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "take two turns", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			if calls.Add(1) == 1 {
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, OutputSummary: "More work remains"}, nil
			}
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done"}, nil
		})}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "agent", MaxTurnsPerClaim: 1,
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var completed *AgentRun
	for time.Now().Before(deadline) {
		completed, err = NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == AgentRunStatusCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed == nil || completed.Status != AgentRunStatusCompleted || completed.Attempt != 2 || calls.Load() != 2 {
		t.Fatalf("run was not fairly yielded and reclaimed: run=%#v calls=%d", completed, calls.Load())
	}
	events, err := NewRunActivityService(store, store).ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	foundYield := false
	for _, event := range events {
		foundYield = foundYield || event.EventType == "run.yielded"
	}
	if !foundYield {
		t.Fatalf("yield activity was not recorded: %#v", events)
	}
}
