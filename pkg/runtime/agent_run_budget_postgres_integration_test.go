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

func TestPostgresAgentRunAttemptBudgetIsReplicaSafeAndRestartDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	schema := "openseal_attempt_budget_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()

	scope := Scope{Kind: "tenant", ID: "attempt-budget"}
	now := time.Now().UTC()
	portfolio := NewPortfolioService(primary)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "execute within one claim", Source: RunSourceObjective, Budget: &BudgetPolicy{MaxAttempts: 1, MaxTurns: 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	claimAcrossReplicas := func(at time.Time, prefix string) []*AgentRun {
		t.Helper()
		stores := []*PostgresStore{primary, replica}
		results := make(chan *AgentRun, len(stores))
		errs := make(chan error, len(stores))
		start := make(chan struct{})
		var wait sync.WaitGroup
		for index, store := range stores {
			wait.Add(1)
			go func(index int, store *PostgresStore) {
				defer wait.Done()
				<-start
				claimed, claimErr := store.ClaimNextAgentRun(ctx, AgentRunClaim{
					Scope: scope, WorkerID: prefix + string(rune('a'+index)), Now: at,
					LeaseDuration: time.Second, AgingInterval: time.Minute,
				})
				results <- claimed
				errs <- claimErr
			}(index, store)
		}
		close(start)
		wait.Wait()
		close(results)
		close(errs)
		for claimErr := range errs {
			if claimErr != nil {
				t.Fatal(claimErr)
			}
		}
		claimed := make([]*AgentRun, 0, 1)
		for candidate := range results {
			if candidate != nil {
				claimed = append(claimed, candidate)
			}
		}
		if len(claimed) != 1 {
			t.Fatalf("replica claims at %s = %d, want 1", at, len(claimed))
		}
		return claimed
	}

	first := claimAcrossReplicas(now, "first-")[0]
	if first.BudgetUsage.Attempts != 1 || first.BudgetState != BudgetStateExhausted {
		t.Fatalf("first attempt = %#v", first)
	}
	recovered := claimAcrossReplicas(now.Add(2*time.Second), "recovery-")[0]
	if recovered.ID != run.ID || recovered.BudgetUsage.Attempts != 2 || !runAttemptBudgetExceeded(recovered) {
		t.Fatalf("recovered attempt = %#v", recovered)
	}
	result, err := NewTurnCoordinator(primary, primary, primary).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: recovered.LeaseOwner,
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		t.Fatal("attempt-exhausted runner executed")
		return nil, nil
	}))
	if !errors.Is(err, ErrBudgetExhausted) || result == nil || result.Run.Status != AgentRunStatusPaused || result.Event == nil || result.Event.EventType != "budget.exhausted" {
		t.Fatalf("attempt enforcement = %#v, %v", result, err)
	}
	loaded, err := NewPortfolioService(replica).GetAgentRun(ctx, scope, run.ID)
	if err != nil || loaded.Status != AgentRunStatusPaused || loaded.BudgetUsage.Attempts != 2 {
		t.Fatalf("replica restart view = %#v, %v", loaded, err)
	}
}

func TestPostgresHostedTurnTokenReservationIsReplicaSafeAndDurable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	schema := "openseal_hosted_budget_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()

	scope := Scope{Kind: "tenant", ID: "hosted-token-budget"}
	run, err := NewPortfolioService(primary).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal:   "execute one bounded hosted turn",
		Budget: &BudgetPolicy{MaxInputTokens: 8000, MaxOutputTokens: 2000, MaxTotalTokens: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &blockingHostedTurnHost{started: make(chan struct{}), release: make(chan struct{})}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, advanceErr := NewTurnCoordinator(primary, primary, primary).Advance(ctx, AdvanceAgentRunRequest{
			Scope: scope, RunID: run.ID, WorkerID: "primary-worker",
		}, runner)
		firstDone <- advanceErr
	}()
	select {
	case <-host.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	persisted, err := NewPortfolioService(replica).GetAgentRun(ctx, scope, run.ID)
	if err != nil || len(persisted.BudgetReservations) != 1 || persisted.BudgetUsage != (BudgetUsage{}) {
		t.Fatalf("replica in-flight budget = %#v, %v", persisted, err)
	}
	_, concurrentErr := NewTurnCoordinator(replica, replica, replica).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: run.ID, WorkerID: "replica-worker",
	}, runner)
	if !errors.Is(concurrentErr, ErrTurnLeaseHeld) {
		t.Fatalf("replica concurrent advance error = %v", concurrentErr)
	}
	host.mu.Lock()
	providerCalls := host.calls
	host.mu.Unlock()
	if providerCalls != 1 {
		t.Fatalf("provider calls during replica contention = %d", providerCalls)
	}
	close(host.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	settled, err := NewPortfolioService(replica).GetAgentRun(ctx, scope, run.ID)
	if err != nil || settled.Status != AgentRunStatusCompleted || settled.BudgetUsage.Turns != 1 || settled.BudgetUsage.InputTokens != 100 || settled.BudgetUsage.OutputTokens != 20 || len(settled.BudgetReservations) != 0 {
		t.Fatalf("replica settled budget = %#v, %v", settled, err)
	}
}
