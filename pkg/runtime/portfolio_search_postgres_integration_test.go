//go:build integration

package runtime

import (
	"testing"
)

func TestPostgresRunSearchIsLiteralAndPrecedesPagination(t *testing.T) {
	store, _ := openTestPostgresStore(t, "openseal_run_search_")
	service := NewPortfolioService(store)
	scope := Scope{Kind: "local", ID: "default"}
	var expected []*AgentRun
	for i, goal := range []string{"Review 50%_ Evidence", "Review 50%_ Evidence", "Review 50%_ Evidence", "Review 50xx Evidence", "Unrelated work"} {
		run, err := service.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, Goal: goal, Source: RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			expected = append(expected, run)
		}
	}
	page, err := service.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, Query: "50%_ eVIDENCE", Order: AgentRunOrderCreatedDesc, Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].ID != expected[1].ID {
		t.Fatalf("search = %#v, %v", page, err)
	}
}
