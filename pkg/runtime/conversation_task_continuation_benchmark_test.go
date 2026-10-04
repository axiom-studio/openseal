package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type conversationTaskDueBenchmarkStore interface {
	conversationTaskSQLTestStore
	ConversationTaskDueStore
}

// Only fixture construction writes history. Timed calls use the production due
// query with its production page limit, including a page after a keyset cursor.
func BenchmarkConversationTaskContinuationDueMemory(b *testing.B) {
	conversationTaskBenchmarkDue(b, func(*testing.B) conversationTaskDueBenchmarkStore { return NewMemoryStore() })
}

func BenchmarkConversationTaskContinuationDueSQLite(b *testing.B) {
	conversationTaskBenchmarkDue(b, func(b *testing.B) conversationTaskDueBenchmarkStore {
		store, err := NewSQLiteStore(filepath.Join(b.TempDir(), "continuation-due.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = store.Close() })
		return store
	})
}

func BenchmarkConversationTaskContinuationDuePostgres(b *testing.B) {
	dsn := os.Getenv("OPENSEAL_TASK_BENCH_POSTGRES_DSN")
	if dsn == "" {
		b.Skip("set OPENSEAL_TASK_BENCH_POSTGRES_DSN to an isolated benchmark database")
	}
	conversationTaskBenchmarkDue(b, func(b *testing.B) conversationTaskDueBenchmarkStore {
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
		return store
	})
}

func conversationTaskBenchmarkDue(b *testing.B, open func(*testing.B) conversationTaskDueBenchmarkStore) {
	for _, history := range []int{100, 10000} {
		b.Run(fmt.Sprintf("completed_%d", history), func(b *testing.B) {
			store := open(b)
			scope := Scope{Kind: "tenant", ID: "continuation-benchmark"}
			base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
			active := make([]*AgentRun, 0, 5)
			// Completed records are older than the live records. A full history
			// scan cannot be hidden by an early LIMIT at the live frontier.
			for index := 0; index < history; index++ {
				conversationTaskBenchmarkDueRun(b, store, scope, fmt.Sprintf("completed-%08d", index), base.Add(time.Duration(index)*time.Microsecond), true)
			}
			for index := 0; index < 5; index++ {
				active = append(active, conversationTaskBenchmarkDueRun(b, store, scope, fmt.Sprintf("active-%08d", index), base.Add(time.Hour+time.Duration(index)*time.Microsecond), false))
			}
			filter := ConversationTaskDueFilter{Scope: scope, BeforeCreatedAt: time.Now().UTC().Add(-ConversationTaskForegroundTimeout), Limit: 100}
			cursor := filter
			cursor.AfterCreatedAt = &active[1].CreatedAt
			cursor.AfterID = active[1].ID
			conversationTaskBenchmarkDueExplain(b, store, filter)
			conversationTaskBenchmarkDueExplain(b, store, cursor)
			b.Run("five_active", func(b *testing.B) { conversationTaskBenchmarkDuePage(b, store, filter, 5) })
			b.Run("three_after_cursor", func(b *testing.B) { conversationTaskBenchmarkDuePage(b, store, cursor, 3) })
			for _, run := range active {
				conversationTaskBenchmarkComplete(b, store, run)
			}
			conversationTaskBenchmarkDueExplain(b, store, filter)
			b.Run("none_same_history", func(b *testing.B) { conversationTaskBenchmarkDuePage(b, store, filter, 0) })
		})
	}
}

func conversationTaskBenchmarkDueRun(b *testing.B, store conversationTaskDueBenchmarkStore, scope Scope, id string, created time.Time, completed bool) *AgentRun {
	b.Helper()
	run := &AgentRun{ID: id, Scope: scope, Kind: RunKindConversation, RootRunID: id, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "benchmark-agent"}, AssignedAgentID: "benchmark-agent",
		ConcurrencyKey: "benchmark-conversation", Goal: "Continue the accepted conversation request", Source: RunSourceChat, Status: AgentRunStatusQueued,
		AvailableAt: created, QueueEnteredAt: created, CreatedAt: created, UpdatedAt: created, Revision: 1,
		Context: map[string]interface{}{conversationRunContextConversationID: "benchmark-conversation", conversationRunContextTriggerID: "benchmark-message-" + id}}
	if completed {
		run.Status = AgentRunStatusCompleted
		run.CompletedAt = &created
	}
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		b.Fatal(err)
	}
	return run
}

func conversationTaskBenchmarkDuePage(b *testing.B, store conversationTaskDueBenchmarkStore, filter ConversationTaskDueFilter, want int) {
	b.Helper()
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		page, err := store.ListDueConversationTaskRuns(context.Background(), filter)
		if err != nil || len(page) != want || len(page) > 100 {
			b.Fatalf("due page=%d want=%d err=%v", len(page), want, err)
		}
	}
	b.ReportMetric(float64(want), "runs/op")
}

func conversationTaskBenchmarkDueExplain(b *testing.B, store conversationTaskDueBenchmarkStore, filter ConversationTaskDueFilter) {
	b.Helper()
	ctx := context.Background()
	switch typed := store.(type) {
	case *SQLiteStore:
		query := `EXPLAIN QUERY PLAN SELECT payload FROM agent_runs WHERE scope_kind=? AND scope_id=? AND ` + sqliteConversationTaskDuePredicate + ` AND created_at<=?`
		args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.BeforeCreatedAt.UTC()}
		if filter.AfterCreatedAt != nil {
			query += ` AND (created_at,id)>(?,?)`
			args = append(args, filter.AfterCreatedAt.UTC(), filter.AfterID)
		}
		query += ` ORDER BY created_at,id LIMIT ?`
		args = append(args, filter.Limit)
		rows, err := typed.db.QueryContext(ctx, query, args...)
		if err != nil {
			b.Fatal(err)
		}
		defer rows.Close()
		found := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				b.Fatal(err)
			}
			b.Logf("SQLite due query: %s", detail)
			if strings.Contains(detail, "idx_agent_runs_conversation_task_due") {
				found = true
			}
			if strings.Contains(detail, "SCAN ") || strings.Contains(detail, "USE TEMP B-TREE") {
				b.Fatalf("unbounded due query plan: %s", detail)
			}
		}
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
		if !found {
			b.Fatal("due query did not use its partial index")
		}
	case *PostgresStore:
		if _, err := typed.db.ExecContext(ctx, "ANALYZE "+typed.table("agent_runs")); err != nil {
			b.Fatal(err)
		}
		query := `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT payload FROM ` + typed.table("agent_runs") + ` WHERE scope_kind=$1 AND scope_id=$2 AND ` + postgresConversationTaskDuePredicate + ` AND created_at<=$3`
		args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.BeforeCreatedAt.UTC()}
		if filter.AfterCreatedAt != nil {
			query += ` AND (created_at,id)>($4,$5) ORDER BY created_at,id LIMIT $6`
			args = append(args, filter.AfterCreatedAt.UTC(), filter.AfterID, filter.Limit)
		} else {
			query += ` ORDER BY created_at,id LIMIT $4`
			args = append(args, filter.Limit)
		}
		var raw []byte
		if err := typed.db.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
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
