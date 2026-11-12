package runtime

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Every ActivityFilter field must narrow a read the same way on every store and in
// both directions; an ascending (per-run) read once ignored all but RunID and
// AfterSequence.
func TestListActivityAppliesEveryFilterInBothDirections(t *testing.T) {
	stores := map[string]func(*testing.T) models.KernelStore{
		"sqlite":   func(t *testing.T) models.KernelStore { return newTestKernelStore(t) },
		"postgres": func(t *testing.T) models.KernelStore { return activityFilterPostgresStore(t) },
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "filters"}
			run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "x"}, AssignedAgentID: "x",
				Goal: "work", Source: models.RunSourceManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
			for i, event := range []*models.ActivityEvent{
				{ID: "e1", EventType: "a", Severity: models.ActivitySeverityInfo, Visibility: models.ActivityVisibilityScope, AgentID: "x", TeamID: "t1"},
				{ID: "e2", EventType: "b", Severity: models.ActivitySeverityWarning, Visibility: models.ActivityVisibilityPrivate, AgentID: "y"},
				{ID: "e3", EventType: "a", Severity: models.ActivitySeverityError, Visibility: models.ActivityVisibilityTeam, AgentID: "x", TeamID: "t2"},
				{ID: "e4", EventType: "c", Severity: models.ActivitySeverityInfo, Visibility: models.ActivityVisibilityScope, AgentID: "x"},
			} {
				event.Scope, event.RunID, event.Summary = scope, run.ID, event.ID
				event.CreatedAt = now.Add(time.Duration(i) * time.Second)
				if _, err := store.AppendActivity(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			before := now.Add(2 * time.Second)
			cases := []struct {
				name   string
				filter models.ActivityFilter
				want   []string // every match, oldest first, before Limit applies
			}{
				{"no filter", models.ActivityFilter{}, []string{"e1", "e2", "e3", "e4"}},
				{"event types", models.ActivityFilter{EventTypes: []string{"a"}}, []string{"e1", "e3"}},
				{"event types with limit", models.ActivityFilter{EventTypes: []string{"a", "c"}, Limit: 2}, []string{"e1", "e3", "e4"}},
				{"severities", models.ActivityFilter{Severities: []models.ActivitySeverity{models.ActivitySeverityInfo}}, []string{"e1", "e4"}},
				{"visibilities", models.ActivityFilter{Visibilities: []models.ActivityVisibility{models.ActivityVisibilityScope, models.ActivityVisibilityTeam}}, []string{"e1", "e3", "e4"}},
				{"agent", models.ActivityFilter{AgentID: "x"}, []string{"e1", "e3", "e4"}},
				{"team", models.ActivityFilter{TeamID: "t2"}, []string{"e3"}},
				{"objective", models.ActivityFilter{ObjectiveID: "other"}, nil},
				{"run ids", models.ActivityFilter{RunIDs: []string{"other"}}, nil},
				{"before created at", models.ActivityFilter{BeforeCreatedAt: &before}, []string{"e1", "e2"}},
			}
			for _, tc := range cases {
				for _, descending := range []bool{false, true} {
					filter := tc.filter
					filter.Scope, filter.RunID, filter.Descending = scope, run.ID, descending
					want := slices.Clone(tc.want)
					if descending {
						slices.Reverse(want)
					}
					if filter.Limit > 0 && filter.Limit < len(want) {
						want = want[:filter.Limit]
					}
					events, err := store.ListActivity(ctx, filter)
					if err != nil {
						t.Fatal(err)
					}
					got := make([]string, 0, len(events))
					for _, event := range events {
						got = append(got, event.ID)
					}
					if !slices.Equal(got, want) {
						t.Errorf("%s (descending=%v) = %v, want %v", tc.name, descending, got, want)
					}
				}
			}
		})
	}
}

func activityFilterPostgresStore(t *testing.T) *postgres.Store {
	t.Helper()
	pgSchema := storetest.NewPostgresSchema(t, "openseal_activity_filter_")
	dsn := pgSchema.DSN
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}
