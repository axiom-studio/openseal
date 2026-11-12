//go:build integration

package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
)

func TestPostgresObjectiveBudgetSerializesReplicaAllocations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	primary, pgSchema := openTestPostgresStore(t, "openseal_budget_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scope := models.Scope{Kind: "tenant", ID: "postgres-budget"}
	objective, err := portfolio.NewPortfolioService(primary).CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Bounded", Goal: "Allocate once",
		Budget: &models.BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	stores := []*postgres.Store{primary, replica}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	var wait sync.WaitGroup
	for index, store := range stores {
		wait.Add(1)
		go func(index int, store *postgres.Store) {
			defer wait.Done()
			<-start
			_, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: objective.Owner.ID,
				Goal: "Replica work", Budget: &models.BudgetPolicy{MaxTurns: 6}, IdempotencyKey: "replica-" + string(rune('a'+index)),
			})
			results <- err
		}(index, store)
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, exhausted := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, models.ErrBudgetExhausted):
			exhausted++
		default:
			t.Fatalf("unexpected replica error: %v", err)
		}
	}
	loaded, err := portfolio.NewPortfolioService(primary).GetObjective(ctx, scope, objective.ID)
	if err != nil || succeeded != 1 || exhausted != 1 || len(loaded.BudgetAllocations) != 1 {
		t.Fatalf("replica allocation success=%d exhausted=%d objective=%#v err=%v", succeeded, exhausted, loaded, err)
	}
}
