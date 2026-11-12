package conversation

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type countedConversationPortfolio struct {
	models.PortfolioStore
	rootCalls, rootRows int
}

func (s *countedConversationPortfolio) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	rows, err := s.PortfolioStore.ListAgentRuns(ctx, filter)
	if filter.Kind == models.RunKindConversation {
		s.rootCalls++
		s.rootRows += len(rows)
	}
	return rows, err
}

func TestConversationRunFilterAvoidsUnrelatedHistory(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, db := newTestSQLiteStore(t)
			scope := models.Scope{Kind: "tenant", ID: "one"}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "writer"}
			now := time.Now().UTC()
			for i := 0; i < 503; i++ {
				run := &models.AgentRun{ID: fmt.Sprintf("run-%d", i), Scope: scope, Kind: models.RunKindConversation, Owner: owner,
					ConcurrencyKey: "unrelated", Goal: "Research", Source: models.RunSourceChat, Status: models.AgentRunStatusCompleted,
					Revision: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now, QueueEnteredAt: now}
				if i >= 500 {
					run.ConcurrencyKey = "selected"
					run.Context = map[string]interface{}{models.ConversationRunConversationIDContextKey: "selected"}
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
			runs, err := service.listConversationRuns(t.Context(), &models.Conversation{ID: "selected", Scope: scope, Owner: owner})
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || runs[0].ID != "run-500" {
				t.Fatalf("unexpected runs: %#v", runs)
			}
			if counted.rootCalls != 1 || counted.rootRows != 1 {
				t.Fatalf("unrelated history fetched: calls=%d rows=%d", counted.rootCalls, counted.rootRows)
			}
			var id, parent, unused int
			var detail string
			err = db.QueryRowContext(t.Context(), `EXPLAIN QUERY PLAN SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND json_extract(payload, '$.concurrencyKey') = ?`, scope.Kind, scope.ID, "selected").Scan(&id, &parent, &unused, &detail)
			if err != nil || !strings.Contains(detail, "idx_agent_runs_concurrency") {
				t.Fatalf("conversation lookup did not use scoped index: %s (%v)", detail, err)
			}
		})
	}
}

func TestConversationRunsIncludeTeamMemberLanes(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store models.KernelStore = kerneltest.NewStore(t)
			if backend == "sqlite" {
				db, err := sqlite.New(filepath.Join(t.TempDir(), "runs.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = db
			}
			scope := models.Scope{Kind: "tenant", ID: "one"}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "launch"}
			now := time.Now().UTC()
			for _, run := range []*models.AgentRun{
				{ID: "routing", ConcurrencyKey: "room"},
				{ID: "member", ConcurrencyKey: "room:agent:rio", AssignedAgentID: "rio"},
				{ID: "forged", ConcurrencyKey: "room:agent:otto", AssignedAgentID: "rio"},
			} {
				run.Scope, run.Kind, run.Owner, run.Goal, run.Source, run.Status = scope, models.RunKindConversation, owner, "Answer", models.RunSourceChat, models.AgentRunStatusRunning
				run.Context = map[string]interface{}{models.ConversationRunConversationIDContextKey: "room", models.ConversationRunTriggerIDContextKey: "ask"}
				run.Revision, run.CreatedAt, run.UpdatedAt, run.AvailableAt, run.QueueEnteredAt = 1, now, now, now, now
				if err := store.CreateAgentRun(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			service := &ConversationChangeService{portfolio: store}
			runs, err := service.listConversationRuns(t.Context(), &models.Conversation{ID: "room", Scope: scope, Owner: owner})
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]bool{}
			for _, run := range runs {
				ids[run.ID] = true
			}
			if len(runs) != 2 || !ids["routing"] || !ids["member"] {
				t.Fatalf("conversation Runs = %v", ids)
			}
		})
	}
}
