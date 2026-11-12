//go:build integration

package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
)

func TestPostgresAgentRunAttemptBudgetIsReplicaSafeAndRestartDurable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	primary, pgSchema := openTestPostgresStore(t, "openseal_attempt_budget_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(schema))
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
		stores := []*postgres.Store{primary, replica}
		results := make(chan *AgentRun, len(stores))
		errs := make(chan error, len(stores))
		start := make(chan struct{})
		var wait sync.WaitGroup
		for index, store := range stores {
			wait.Add(1)
			go func(index int, store *postgres.Store) {
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
	if recovered.ID != run.ID || recovered.Status != AgentRunStatusPaused || recovered.BudgetUsage.Attempts != 1 ||
		!runAttemptBudgetAtLimit(recovered) || runAttemptBudgetExceeded(recovered) {
		t.Fatalf("recovered attempt = %#v", recovered)
	}
	loaded, err := NewPortfolioService(replica).GetAgentRun(ctx, scope, run.ID)
	if err != nil || loaded.Status != AgentRunStatusPaused || loaded.BudgetUsage.Attempts != 1 {
		t.Fatalf("replica restart view = %#v, %v", loaded, err)
	}
}
