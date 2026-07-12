package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMemoryAgentTurnsEnforceOneActiveTurn(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	portfolio := NewPortfolioService(store)
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	turns := NewAgentTurnService(store, store)
	const count = 20
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			turn, beginErr := turns.BeginTurn(ctx, BeginAgentTurnRequest{
				Scope: scope, RunID: run.ID, Model: "test", WorkerID: fmt.Sprintf("worker-%d", i),
			})
			_ = turn
			errs <- beginErr
		}(i)
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrActiveTurnExists) {
			t.Fatalf("unexpected begin error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent turn starts = %d, want 1", successes)
	}
	listed, err := turns.ListTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID, Limit: count})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Sequence != 1 {
		t.Fatalf("unexpected turn list: %#v", listed)
	}
	first := listed[0]
	finished, err := turns.FinishTurn(ctx, scope, first.ID, FinishAgentTurnRequest{
		ExpectedRevision: first.Revision, Status: AgentTurnStatusCompleted, WorkerID: first.LeaseOwner,
		ModelProvider: "openai-compatible", Model: "actual-model",
		Decisions:              []TurnDecision{{Summary: "Continue", Rationale: "Evidence supports it"}},
		ContinuationCheckpoint: map[string]interface{}{"step": float64(2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Revision != 2 || finished.CompletedAt == nil || finished.ContinuationCheckpoint["step"] != float64(2) || finished.ModelProvider != "openai-compatible" || finished.Model != "actual-model" {
		t.Fatalf("turn did not finish durably: %#v", finished)
	}
	if _, err := turns.FinishTurn(ctx, scope, first.ID, FinishAgentTurnRequest{
		ExpectedRevision: first.Revision, Status: AgentTurnStatusCompleted, WorkerID: first.LeaseOwner,
	}); err == nil {
		t.Fatal("stale finish unexpectedly succeeded")
	}
	second, err := turns.BeginTurn(ctx, BeginAgentTurnRequest{Scope: scope, RunID: run.ID, Model: "test", WorkerID: "worker-next"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 {
		t.Fatalf("second turn sequence = %d, want 2", second.Sequence)
	}
	turns.now = func() time.Time { return second.LeaseExpiresAt.Add(-time.Second) }
	if _, err := turns.ClaimTurn(ctx, scope, second.ID, "recovery-worker", time.Minute); !errors.Is(err, ErrTurnLeaseHeld) {
		t.Fatalf("live lease claim error = %v", err)
	}
	turns.now = func() time.Time { return second.LeaseExpiresAt.Add(time.Second) }
	reclaimed, err := turns.ClaimTurn(ctx, scope, second.ID, "recovery-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.LeaseOwner != "recovery-worker" || reclaimed.Revision != second.Revision+1 {
		t.Fatalf("unexpected reclaimed turn: %#v", reclaimed)
	}
}
