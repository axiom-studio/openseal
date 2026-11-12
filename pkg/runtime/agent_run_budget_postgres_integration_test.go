//go:build integration

package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
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

	scope := models.Scope{Kind: "tenant", ID: "attempt-budget"}
	now := time.Now().UTC()
	portfolio := runtimeportfolio.NewPortfolioService(primary, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "execute within one claim", Source: models.RunSourceObjective, Budget: &models.BudgetPolicy{MaxAttempts: 1, MaxTurns: 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	claimAcrossReplicas := func(at time.Time, prefix string) []*models.AgentRun {
		t.Helper()
		stores := []*postgres.Store{primary, replica}
		results := make(chan *models.AgentRun, len(stores))
		errs := make(chan error, len(stores))
		start := make(chan struct{})
		var wait sync.WaitGroup
		for index, store := range stores {
			wait.Add(1)
			go func(index int, store *postgres.Store) {
				defer wait.Done()
				<-start
				claimed, claimErr := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
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
		claimed := make([]*models.AgentRun, 0, 1)
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
	if first.BudgetUsage.Attempts != 1 || first.BudgetState != models.BudgetStateExhausted {
		t.Fatalf("first attempt = %#v", first)
	}
	recovered := claimAcrossReplicas(now.Add(2*time.Second), "recovery-")[0]
	if recovered.ID != run.ID || recovered.Status != models.AgentRunStatusPaused || recovered.BudgetUsage.Attempts != 1 ||
		!models.RunAttemptBudgetAtLimit(recovered) || models.RunAttemptBudgetExceeded(recovered) {
		t.Fatalf("recovered attempt = %#v", recovered)
	}
	loaded, err := runtimeportfolio.NewPortfolioService(replica).GetAgentRun(ctx, scope, run.ID)
	if err != nil || loaded.Status != models.AgentRunStatusPaused || loaded.BudgetUsage.Attempts != 1 {
		t.Fatalf("replica restart view = %#v, %v", loaded, err)
	}
}
