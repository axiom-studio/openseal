package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAgentRunCreatedDescendingOrderPrecedesPagination(t *testing.T) {
	stores := map[string]func(*testing.T) PortfolioStore{
		"memory": func(*testing.T) PortfolioStore { return NewMemoryStore(100) },
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
