//go:build integration

package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresConversationRunLookupUpgrade(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	schema := "conversation_lookup_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	scope := Scope{Kind: "tenant", ID: "one"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	now := time.Now().UTC()
	for _, id := range []string{"match", "other-chat", "other-tenant", "other-owner"} {
		run := &AgentRun{ID: id, Scope: scope, Owner: owner, Kind: RunKindConversation, ConcurrencyKey: "chat",
			Goal: "Research", Source: RunSourceChat, Status: AgentRunStatusCompleted, Revision: 1,
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
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname='agent_runs_concurrency_idx'`, schema).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback index count=%d: %v", count, err)
	}
	upgraded, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err := upgraded.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname='agent_runs_concurrency_idx'`, schema).Scan(&count); err != nil || count != 1 {
		t.Fatalf("upgrade index count=%d: %v", count, err)
	}
	runs, err := upgraded.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Owner: &owner, Kind: RunKindConversation, ConcurrencyKey: "chat", Limit: 1})
	if err != nil || len(runs) != 1 || runs[0].ID != "match" {
		t.Fatalf("wrong scoped runs: %#v, %v", runs, err)
	}
	tx, err := upgraded.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	var plan string
	if err := tx.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) SELECT payload FROM `+upgraded.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND payload->>'concurrencyKey'=$3`, scope.Kind, scope.ID, "chat").Scan(&plan); err != nil || !strings.Contains(plan, "agent_runs_concurrency_idx") {
		t.Fatalf("index is not usable: %s, %v", plan, err)
	}
}
