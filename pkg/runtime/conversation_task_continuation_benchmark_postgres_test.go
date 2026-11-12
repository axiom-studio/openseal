package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Postgres store benchmark: it reads the store's due predicate, so it moves
// to the Postgres store package with the store.

func BenchmarkConversationTaskContinuationDuePostgres(b *testing.B) {
	dsn := os.Getenv("OPENSEAL_TASK_BENCH_POSTGRES_DSN")
	if dsn == "" {
		b.Skip("set OPENSEAL_TASK_BENCH_POSTGRES_DSN to an isolated benchmark database")
	}
	storetest.BenchmarkConversationTaskDue(b, func(b *testing.B) (storetest.ConversationTaskDueStore, storetest.ConversationTaskDueExplain) {
		schema := fmt.Sprintf("conversation_continuation_benchmark_%x", time.Now().UnixNano())
		store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema(schema))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if _, err := store.db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+store.quotedSchema()+" CASCADE"); err != nil {
				b.Error(err)
			}
			_ = store.Close()
		})
		return store, func(b *testing.B, filter ConversationTaskDueFilter) {
			explainPostgresConversationTaskDue(b, store, filter)
		}
	})
}

func explainPostgresConversationTaskDue(b *testing.B, store *PostgresStore, filter ConversationTaskDueFilter) {
	b.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "ANALYZE "+store.table("agent_runs")); err != nil {
		b.Fatal(err)
	}
	query := `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT payload FROM ` + store.table("agent_runs") + ` WHERE scope_kind=$1 AND scope_id=$2 AND ` + postgresConversationTaskDuePredicate + ` AND created_at<=$3`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.BeforeCreatedAt.UTC()}
	if filter.AfterCreatedAt != nil {
		query += ` AND (created_at,id)>($4,$5) ORDER BY created_at,id LIMIT $6`
		args = append(args, filter.AfterCreatedAt.UTC(), filter.AfterID, filter.Limit)
	} else {
		query += ` ORDER BY created_at,id LIMIT $4`
		args = append(args, filter.Limit)
	}
	var raw []byte
	if err := store.db.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		b.Fatal(err)
	}
	var plans []map[string]interface{}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		b.Fatalf("invalid due query plan: %s %v", raw, err)
	}
	found := conversationTaskBenchmarkDuePostgresPlan(b, plans[0]["Plan"])
	if !found {
		b.Fatalf("due query did not use its partial index: %s", raw)
	}
}

func conversationTaskBenchmarkDuePostgresPlan(b *testing.B, value interface{}) bool {
	b.Helper()
	node, ok := value.(map[string]interface{})
	if !ok {
		b.Fatalf("invalid PostgreSQL plan node: %#v", value)
	}
	if node["Node Type"] == "Seq Scan" {
		b.Fatalf("due query scanned retained history: %#v", node)
	}
	b.Logf("PostgreSQL due query: type=%v index=%v rows=%v loops=%v", node["Node Type"], node["Index Name"], node["Actual Rows"], node["Actual Loops"])
	found := node["Index Name"] == "agent_runs_conversation_task_due_idx"
	if children, ok := node["Plans"].([]interface{}); ok {
		for _, child := range children {
			if conversationTaskBenchmarkDuePostgresPlan(b, child) {
				found = true
			}
		}
	}
	return found
}
