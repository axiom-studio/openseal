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

func TestPostgresObjectiveBudgetSerializesReplicaAllocations(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	schema := "openseal_budget_" + uuid.NewString()[:8]
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
	scope := Scope{Kind: "tenant", ID: "postgres-budget"}
	objective, err := NewPortfolioService(primary).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Bounded", Goal: "Allocate once",
		Budget: &BudgetPolicy{MaxTurns: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	stores := []*PostgresStore{primary, replica}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	var wait sync.WaitGroup
	for index, store := range stores {
		wait.Add(1)
		go func(index int, store *PostgresStore) {
			defer wait.Done()
			<-start
			_, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: objective.Owner.ID,
				Goal: "Replica work", Budget: &BudgetPolicy{MaxTurns: 6}, IdempotencyKey: "replica-" + string(rune('a'+index)),
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
		case errors.Is(err, ErrBudgetExhausted):
			exhausted++
		default:
			t.Fatalf("unexpected replica error: %v", err)
		}
	}
	loaded, err := NewPortfolioService(primary).GetObjective(ctx, scope, objective.ID)
	if err != nil || succeeded != 1 || exhausted != 1 || len(loaded.BudgetAllocations) != 1 {
		t.Fatalf("replica allocation success=%d exhausted=%d objective=%#v err=%v", succeeded, exhausted, loaded, err)
	}
}
