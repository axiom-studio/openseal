//go:build integration

package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestPostgresRunSearchIsLiteralAndPrecedesPagination(t *testing.T) {
	store, _ := kerneltest.OpenPostgres(t, "openseal_run_search_")
	service := portfolio.NewPortfolioService(store)
	scope := models.Scope{Kind: "local", ID: "default"}
	var expected []*models.AgentRun
	for i, goal := range []string{"Review 50%_ Evidence", "Review 50%_ Evidence", "Review 50%_ Evidence", "Review 50xx Evidence", "Unrelated work"} {
		run, err := service.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "analyst"}, Goal: goal, Source: models.RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			expected = append(expected, run)
		}
	}
	page, err := service.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, Query: "50%_ eVIDENCE", Order: models.AgentRunOrderCreatedDesc, Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].ID != expected[1].ID {
		t.Fatalf("search = %#v, %v", page, err)
	}
}
