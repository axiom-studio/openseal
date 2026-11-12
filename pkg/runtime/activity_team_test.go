package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestTeamOwnedRunEventsPopulateCanonicalTeamProjection(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "team-activity.db"))
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
			scope := models.Scope{Kind: "tenant", ID: "acme"}
			run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marketing",
				Goal: "Launch the release", Source: models.RunSourceManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, event, err := activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{
				ExpectedRevision: run.Revision, Status: models.AgentRunStatusPlanning, Summary: "Planning the launch",
				Actor: models.ActivityActor{Type: "agent", ID: "marketing"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if event.TeamID != "gtm" {
				t.Fatalf("event team id = %q", event.TeamID)
			}
			feed, err := activity.NewRunActivityService(store, store).ListActivityFeed(ctx, activity.ActivityFeedRequest{
				Scope: scope, TeamID: "gtm", Limit: 10,
			})
			if err != nil || len(feed.Items) != 1 || feed.Items[0].TeamID != "gtm" || feed.Items[0].Summary != "Planning the launch" {
				t.Fatalf("team feed = %#v, %v", feed, err)
			}
		})
	}
}
