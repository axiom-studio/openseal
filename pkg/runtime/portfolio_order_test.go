package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAgentRunCreatedDescendingOrderPrecedesPagination(t *testing.T) {
	stores := map[string]func(*testing.T) PortfolioStore{
		"memory": func(*testing.T) PortfolioStore { return NewMemoryStore() },
		"sqlite": func(t *testing.T) PortfolioStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	for name, createStore := range stores {
		t.Run(name, func(t *testing.T) {
			service := NewPortfolioService(createStore(t))
			scope := Scope{Kind: "tenant", ID: "7"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "42"}
			created := make([]*AgentRun, 0, 3)
			for _, priority := range []int{100, 50, 1} {
				run, err := service.CreateAgentRun(context.Background(), CreateAgentRunRequest{
					Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "work", Source: RunSourceManual, Priority: priority,
				})
				if err != nil {
					t.Fatal(err)
				}
				created = append(created, run)
			}
			latest, err := service.ListAgentRuns(context.Background(), AgentRunFilter{
				Scope: scope, Owner: &owner, Order: AgentRunOrderCreatedDesc, Limit: 2,
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
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store PortfolioStore = NewMemoryStore()
			if kind == "sqlite" {
				db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "search.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				store = db
			}
			service := NewPortfolioService(store)
			scope := Scope{Kind: "local", ID: "default"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}
			var expected []*AgentRun
			for i := 0; i < 105; i++ {
				goal := "Unrelated work"
				if i < 3 {
					goal = "Review 50%_ of Evidence"
				}
				run, err := service.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: goal, Source: RunSourceManual})
				if err != nil {
					t.Fatal(err)
				}
				if i < 3 {
					expected = append(expected, run)
				}
			}
			page, err := service.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, Query: "  50%_ OF eVIDENCE  ", Order: AgentRunOrderCreatedDesc, Limit: 1, Offset: 1})
			if err != nil || len(page) != 1 || page[0].ID != expected[1].ID {
				t.Fatalf("search page = %#v, %v", page, err)
			}
			empty, err := service.ListAgentRuns(t.Context(), AgentRunFilter{Scope: Scope{Kind: "local", ID: "other"}, Query: "Evidence"})
			if err != nil || len(empty) != 0 {
				t.Fatalf("cross-scope search = %#v, %v", empty, err)
			}
			empty, err = service.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, Query: "Evidence", Statuses: []AgentRunStatus{AgentRunStatusCompleted}})
			if err != nil || len(empty) != 0 {
				t.Fatalf("status search = %#v, %v", empty, err)
			}
		})
	}
}
