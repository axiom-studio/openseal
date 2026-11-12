package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func BenchmarkConversationTaskContinuationDueSQLite(b *testing.B) {
	storetest.BenchmarkConversationTaskDue(b, func(b *testing.B) (storetest.ConversationTaskDueStore, storetest.ConversationTaskDueExplain) {
		store, err := NewSQLiteStore(filepath.Join(b.TempDir(), "continuation-due.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = store.Close() })
		return store, func(b *testing.B, filter models.ConversationTaskDueFilter) {
			explainSQLiteConversationTaskDue(b, store, filter)
		}
	})
}

func explainSQLiteConversationTaskDue(b *testing.B, store *Store, filter models.ConversationTaskDueFilter) {
	b.Helper()
	query := `EXPLAIN QUERY PLAN SELECT payload FROM agent_runs WHERE scope_kind=? AND scope_id=? AND ` + sqliteConversationTaskDuePredicate + ` AND created_at<=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.BeforeCreatedAt.UTC()}
	if filter.AfterCreatedAt != nil {
		query += ` AND (created_at,id)>(?,?)`
		args = append(args, filter.AfterCreatedAt.UTC(), filter.AfterID)
	}
	query += ` ORDER BY created_at,id LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := store.db.QueryContext(context.Background(), query, args...)
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
}
