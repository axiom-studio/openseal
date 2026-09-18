package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type countedConversationPortfolio struct {
	PortfolioStore
	rootCalls, rootRows int
}

func (s *countedConversationPortfolio) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	rows, err := s.PortfolioStore.ListAgentRuns(ctx, filter)
	if filter.Kind == RunKindConversation {
		s.rootCalls++
		s.rootRows += len(rows)
	}
	return rows, err
}

func TestConversationRunFilterAvoidsUnrelatedHistory(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store KernelStore = NewMemoryStore()
			if backend == "sqlite" {
				db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "runs.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = db
			}
			scope := Scope{Kind: "tenant", ID: "one"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "writer"}
			now := time.Now().UTC()
			for i := 0; i < 503; i++ {
				run := &AgentRun{ID: fmt.Sprintf("run-%d", i), Scope: scope, Kind: RunKindConversation, Owner: owner,
					ConcurrencyKey: "unrelated", Goal: "Research", Source: RunSourceChat, Status: AgentRunStatusCompleted,
					Revision: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now, QueueEnteredAt: now}
				if i >= 500 {
					run.ConcurrencyKey = "selected"
				}
				if i == 501 {
					run.Scope.ID = "foreign"
				}
				if i == 502 {
					run.Owner.ID = "other-agent"
				}
				if err := store.CreateAgentRun(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			counted := &countedConversationPortfolio{PortfolioStore: store}
			service := &ConversationChangeService{portfolio: counted}
			runs, err := service.listConversationRuns(t.Context(), &Conversation{ID: "selected", Scope: scope, Owner: owner})
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || runs[0].ID != "run-500" {
				t.Fatalf("unexpected runs: %#v", runs)
			}
			if counted.rootCalls != 1 || counted.rootRows != 1 {
				t.Fatalf("unrelated history fetched: calls=%d rows=%d", counted.rootCalls, counted.rootRows)
			}
			if db, ok := store.(*SQLiteStore); ok {
				var id, parent, unused int
				var detail string
				err := db.db.QueryRowContext(t.Context(), `EXPLAIN QUERY PLAN SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND json_extract(payload, '$.concurrencyKey') = ?`, scope.Kind, scope.ID, "selected").Scan(&id, &parent, &unused, &detail)
				if err != nil || !strings.Contains(detail, "idx_agent_runs_concurrency") {
					t.Fatalf("conversation lookup did not use scoped index: %s (%v)", detail, err)
				}
			}
		})
	}
}
