//go:build integration

package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresObjectiveLifecycleCommitsActivityAtomically(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "openseal_objective_activity_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	portfolio := NewPortfolioService(store)
	scope := Scope{Kind: "tenant", ID: "audit"}
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Audit", Goal: "Stay visible",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := ObjectiveStatusActive
	objective, err = portfolio.UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{ExpectedRevision: objective.Revision, Status: &active})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
	if err != nil || len(events) != 2 || events[0].EventType != "objective.status_changed" || events[1].EventType != "objective.created" {
		t.Fatalf("events = %#v, err = %v", events, err)
	}
}
