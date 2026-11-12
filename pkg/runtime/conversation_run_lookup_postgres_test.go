//go:build integration

package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresConversationRunLookupUpgrade(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "conversation_lookup_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	scope := models.Scope{Kind: "tenant", ID: "one"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}
	now := time.Now().UTC()
	for _, id := range []string{"match", "other-chat", "other-tenant", "other-owner"} {
		run := &models.AgentRun{ID: id, Scope: scope, Owner: owner, Kind: models.RunKindConversation, ConcurrencyKey: "chat",
			Goal: "Research", Source: models.RunSourceChat, Status: models.AgentRunStatusCompleted, Revision: 1,
			CreatedAt: now, UpdatedAt: now, AvailableAt: now, QueueEnteredAt: now}
		switch id {
		case "other-chat":
			run.ConcurrencyKey = "another"
		case "other-tenant":
			run.Scope.ID = "two"
		case "other-owner":
			run.Owner.ID = "another"
		}
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RollbackPostgresMigrations(ctx, conversationRunLookupMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname='agent_runs_concurrency_idx'`, schema).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback index count=%d: %v", count, err)
	}
	upgraded, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname='agent_runs_concurrency_idx'`, schema).Scan(&count); err != nil || count != 1 {
		t.Fatalf("upgrade index count=%d: %v", count, err)
	}
	runs, err := upgraded.ListAgentRuns(ctx, models.AgentRunFilter{Scope: scope, Owner: &owner, Kind: models.RunKindConversation, ConcurrencyKey: "chat", Limit: 1})
	if err != nil || len(runs) != 1 || runs[0].ID != "match" {
		t.Fatalf("wrong scoped runs: %#v, %v", runs, err)
	}
	tx, err := pgSchema.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	var plan string
	if err := tx.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) SELECT payload FROM `+pgSchema.Table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND payload->>'concurrencyKey'=$3`, scope.Kind, scope.ID, "chat").Scan(&plan); err != nil || !strings.Contains(plan, "agent_runs_concurrency_idx") {
		t.Fatalf("index is not usable: %s, %v", plan, err)
	}
}
