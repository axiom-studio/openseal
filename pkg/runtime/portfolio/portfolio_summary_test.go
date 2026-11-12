package portfolio_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestPortfolioSummarizesRunsByRequestedOwner(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) models.KernelStore
	}{
		{name: "sqlite", store: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "summary.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := portfolio.NewService(test.store(t))
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "one"}
			first := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "38"}
			second := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "39"}
			for index := 0; index < 2; index++ {
				if _, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Owner: first, AssignedAgentID: first.ID, Goal: "work", Source: models.RunSourceManual}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Owner: second, AssignedAgentID: second.ID, Goal: "other", Source: models.RunSourceManual}); err != nil {
				t.Fatal(err)
			}
			summaries, err := service.SummarizeAgentRuns(ctx, scope, []models.ObjectiveOwner{first, {Type: models.OwnerTypeAgent, ID: "missing"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(summaries) != 1 || summaries[0].Owner != first || summaries[0].RunCount != 2 || summaries[0].LastRunAt == nil || summaries[0].LastStatus != models.AgentRunStatusQueued {
				t.Fatalf("summaries = %#v", summaries)
			}
		})
	}
}
