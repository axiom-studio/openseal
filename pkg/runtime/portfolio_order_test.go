package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestAgentRunCreatedDescendingOrderPrecedesPagination(t *testing.T) {
	stores := map[string]func(*testing.T) models.PortfolioStore{
		"sqlite": func(t *testing.T) models.PortfolioStore {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	for name, createStore := range stores {
		t.Run(name, func(t *testing.T) {
			service := portfolio.NewPortfolioService(createStore(t))
			scope := models.Scope{Kind: "tenant", ID: "7"}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "42"}
			created := make([]*models.AgentRun, 0, 3)
			for _, priority := range []int{100, 50, 1} {
				run, err := service.CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
					Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "work", Source: models.RunSourceManual, Priority: priority,
				})
				if err != nil {
					t.Fatal(err)
				}
				created = append(created, run)
			}
			latest, err := service.ListAgentRuns(context.Background(), models.AgentRunFilter{
				Scope: scope, Owner: &owner, Order: models.AgentRunOrderCreatedDesc, Limit: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(latest) != 2 || latest[0].ID != created[2].ID || latest[1].ID != created[1].ID {
				t.Fatalf("created-desc page = %#v", latest)
			}
		})
	}
}

func TestAgentRunSearchPrecedesPaginationAndIsLiteral(t *testing.T) {
	for _, kind := range []string{"sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store models.PortfolioStore = newTestKernelStore(t)
			if kind == "sqlite" {
				db, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "search.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				store = db
			}
			service := portfolio.NewPortfolioService(store)
			scope := models.Scope{Kind: "local", ID: "default"}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "analyst"}
			var expected []*models.AgentRun
			for i := 0; i < 105; i++ {
				goal := "Unrelated work"
				if i < 3 {
					goal = "Review 50%_ of Evidence"
				}
				run, err := service.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: goal, Source: models.RunSourceManual})
				if err != nil {
					t.Fatal(err)
				}
				if i < 3 {
					expected = append(expected, run)
				}
			}
			page, err := service.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, Query: "  50%_ OF eVIDENCE  ", Order: models.AgentRunOrderCreatedDesc, Limit: 1, Offset: 1})
			if err != nil || len(page) != 1 || page[0].ID != expected[1].ID {
				t.Fatalf("search page = %#v, %v", page, err)
			}
			empty, err := service.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: models.Scope{Kind: "local", ID: "other"}, Query: "Evidence"})
			if err != nil || len(empty) != 0 {
				t.Fatalf("cross-scope search = %#v, %v", empty, err)
			}
			empty, err = service.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, Query: "Evidence", Statuses: []models.AgentRunStatus{models.AgentRunStatusCompleted}})
			if err != nil || len(empty) != 0 {
				t.Fatalf("status search = %#v, %v", empty, err)
			}
		})
	}
}
