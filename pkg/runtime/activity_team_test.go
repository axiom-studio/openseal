package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTeamOwnedRunEventsPopulateCanonicalTeamProjection(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(20), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "team-activity.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, closeStore := tc.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "acme"}
			run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marketing",
				Goal: "Launch the release", Source: RunSourceManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, event, err := NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
				ExpectedRevision: run.Revision, Status: AgentRunStatusPlanning, Summary: "Planning the launch",
				Actor: ActivityActor{Type: "agent", ID: "marketing"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if event.TeamID != "gtm" {
				t.Fatalf("event team id = %q", event.TeamID)
			}
			feed, err := NewRunActivityService(store, store).ListActivityFeed(ctx, ActivityFeedRequest{
				Scope: scope, TeamID: "gtm", Limit: 10,
			})
			if err != nil || len(feed.Items) != 1 || feed.Items[0].TeamID != "gtm" || feed.Items[0].Summary != "Planning the launch" {
				t.Fatalf("team feed = %#v, %v", feed, err)
			}
		})
	}
}
