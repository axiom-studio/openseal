package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPortfolioSummarizesRunsByRequestedOwner(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "summary.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewPortfolioService(test.store(t))
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "one"}
			first := ObjectiveOwner{Type: OwnerTypeAgent, ID: "38"}
			second := ObjectiveOwner{Type: OwnerTypeAgent, ID: "39"}
			for index := 0; index < 2; index++ {
				if _, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: first, AssignedAgentID: first.ID, Goal: "work", Source: RunSourceManual}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: second, AssignedAgentID: second.ID, Goal: "other", Source: RunSourceManual}); err != nil {
				t.Fatal(err)
			}
			summaries, err := service.SummarizeAgentRuns(ctx, scope, []ObjectiveOwner{first, {Type: OwnerTypeAgent, ID: "missing"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(summaries) != 1 || summaries[0].Owner != first || summaries[0].RunCount != 2 || summaries[0].LastRunAt == nil || summaries[0].LastStatus != AgentRunStatusQueued {
				t.Fatalf("summaries = %#v", summaries)
			}
		})
	}
}
