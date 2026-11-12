package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestAgentTurnsEnforceOneActiveTurn(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "test"}
	portfolio := portfolio.NewPortfolioService(store)
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "work", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	turnsClock := time.Now
	turns := activity.NewAgentTurnService(store, store, models.WithClock(func() time.Time { return turnsClock() }))
	const count = 20
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			turn, beginErr := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{
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
		} else if !errors.Is(err, models.ErrActiveTurnExists) {
			t.Fatalf("unexpected begin error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent turn starts = %d, want 1", successes)
	}
	listed, err := turns.ListTurns(ctx, models.AgentTurnFilter{Scope: scope, RunID: run.ID, Limit: count})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Sequence != 1 {
		t.Fatalf("unexpected turn list: %#v", listed)
	}
	first := listed[0]
	finished, err := turns.FinishTurn(ctx, scope, first.ID, activity.FinishAgentTurnRequest{
		ExpectedRevision: first.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: first.LeaseOwner,
		OutputSummary: "Step complete", Usage: models.TurnUsage{InputTokens: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Revision != 2 || finished.CompletedAt == nil || finished.OutputSummary != "Step complete" || finished.Usage.InputTokens != 7 || finished.LeaseOwner != "" {
		t.Fatalf("turn did not finish durably: %#v", finished)
	}
	if _, err := turns.FinishTurn(ctx, scope, first.ID, activity.FinishAgentTurnRequest{
		ExpectedRevision: first.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: first.LeaseOwner,
	}); err == nil {
		t.Fatal("stale finish unexpectedly succeeded")
	}
	second, err := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{Scope: scope, RunID: run.ID, Model: "test", WorkerID: "worker-next"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 {
		t.Fatalf("second turn sequence = %d, want 2", second.Sequence)
	}
	turnsClock = func() time.Time { return second.LeaseExpiresAt.Add(-time.Second) }
	if _, err := turns.ClaimTurn(ctx, scope, second.ID, "recovery-worker", time.Minute); !errors.Is(err, models.ErrTurnLeaseHeld) {
		t.Fatalf("live lease claim error = %v", err)
	}
	turnsClock = func() time.Time { return second.LeaseExpiresAt.Add(time.Second) }
	reclaimed, err := turns.ClaimTurn(ctx, scope, second.ID, "recovery-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.LeaseOwner != "recovery-worker" || reclaimed.Revision != second.Revision+1 {
		t.Fatalf("unexpected reclaimed turn: %#v", reclaimed)
	}
}
