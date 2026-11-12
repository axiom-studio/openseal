package activity

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestActivityFeedProjectsStableSummaryFirstPages(t *testing.T) {
	stores := []struct {
		name  string
		store models.KernelStore
		close func()
	}{}
	sqlite, err := sqlite.New(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	stores = append(stores, struct {
		name  string
		store models.KernelStore
		close func()
	}{name: "sqlite", store: sqlite, close: func() { _ = sqlite.Close() }})

	for _, fixture := range stores {
		t.Run(fixture.name, func(t *testing.T) {
			defer fixture.close()
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "one"}
			now := time.Date(2026, 7, 10, 7, 0, 0, 0, time.UTC)
			for _, run := range []*models.AgentRun{
				activityFeedRun("run-one", scope, "agent-one", now),
				activityFeedRun("run-two", scope, "agent-one", now),
				activityFeedRun("run-other", scope, "agent-two", now),
			} {
				if err := fixture.store.CreateAgentRun(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			service := NewRunActivityService(fixture.store, fixture.store)
			for _, event := range []*models.ActivityEvent{
				{ID: "event-a", Scope: scope, RunID: "run-one", AgentID: "agent-one", EventType: "run.claimed", Summary: "Claimed", Actor: models.ActivityActor{Type: "worker", ID: "one"}, Visibility: models.ActivityVisibilityScope, CreatedAt: now, Payload: map[string]interface{}{"evidence": "artifact://one"}},
				{ID: "event-b", Scope: scope, RunID: "run-two", AgentID: "agent-one", EventType: "action.succeeded", Summary: "Published", Actor: models.ActivityActor{Type: "agent", ID: "agent-one"}, Visibility: models.ActivityVisibilityTeam, CreatedAt: now, CorrelationID: "correlation", UsageDelta: &models.BudgetUsage{Turns: 1, InputTokens: 120, OutputTokens: 30, CostMicros: 125_000, DurationMS: 500}},
				{ID: "event-c", Scope: scope, RunID: "run-other", AgentID: "agent-two", EventType: "run.completed", Summary: "Other", Actor: models.ActivityActor{Type: "agent", ID: "agent-two"}, Visibility: models.ActivityVisibilityScope, CreatedAt: now.Add(time.Second)},
				{ID: "event-private", Scope: scope, RunID: "run-one", AgentID: "agent-one", EventType: "turn.reasoned", Summary: "Private", Actor: models.ActivityActor{Type: "agent", ID: "agent-one"}, Visibility: models.ActivityVisibilityPrivate, CreatedAt: now.Add(2 * time.Second)},
			} {
				if _, err := service.AppendActivity(ctx, event); err != nil {
					t.Fatal(err)
				}
			}

			page, err := service.ListActivityFeed(ctx, FeedRequest{
				Scope: scope, AgentID: "agent-one", Limit: 1,
				Visibilities: []models.ActivityVisibility{models.ActivityVisibilityScope, models.ActivityVisibilityTeam},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].ID != "event-b" || page.Items[0].Category != "action" ||
				page.Items[0].CorrelationID != "" || page.Items[0].Payload != nil || !page.Items[0].DetailAvailable || !page.HasMore || page.NextCursor == "" ||
				page.Items[0].UsageDelta == nil || page.Items[0].UsageDelta.InputTokens != 120 || page.Items[0].UsageDelta.OutputTokens != 30 ||
				page.Items[0].UsageDelta.CostMicros != 125_000 || page.Items[0].UsageDelta.DurationMS != 500 {
				t.Fatalf("summary page = %#v", page)
			}
			next, err := service.ListActivityFeed(ctx, FeedRequest{
				Scope: scope, AgentID: "agent-one", Cursor: page.NextCursor, Limit: 1, IncludeDetails: true,
				Visibilities: []models.ActivityVisibility{models.ActivityVisibilityScope, models.ActivityVisibilityTeam},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(next.Items) != 1 || next.Items[0].ID != "event-a" || next.Items[0].Payload["evidence"] != "artifact://one" || next.HasMore {
				t.Fatalf("detail page = %#v", next)
			}
		})
	}
}

func TestActivityFeedRequiresSelectorAndRejectsInvalidCursor(t *testing.T) {
	store := kerneltest.NewStore(t)
	service := NewRunActivityService(store, store)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	if _, err := service.ListActivityFeed(context.Background(), FeedRequest{Scope: scope}); err == nil {
		t.Fatal("selector-free activity feed was accepted")
	}
	if _, err := service.ListActivityFeed(context.Background(), FeedRequest{Scope: scope, AgentID: "agent", Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("invalid activity cursor was accepted")
	}
	if _, err := service.ListActivityFeed(context.Background(), FeedRequest{Scope: scope, AgentID: "agent", Limit: maximumActivityFeedLimit + 1}); err == nil {
		t.Fatal("oversized activity page was accepted")
	}
}

func TestExistingRunActivityStreamKeepsSequenceCursor(t *testing.T) {
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC()
	if err := store.CreateAgentRun(context.Background(), activityFeedRun("run", scope, "agent", now)); err != nil {
		t.Fatal(err)
	}
	service := NewRunActivityService(store, store)
	for _, id := range []string{"first", "second"} {
		if _, err := service.AppendActivity(context.Background(), &models.ActivityEvent{
			ID: id, Scope: scope, RunID: "run", AgentID: "agent", EventType: "run.updated", Summary: id,
			Actor: models.ActivityActor{Type: "agent", ID: "agent"}, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := service.ListActivity(context.Background(), models.ActivityFilter{Scope: scope, RunID: "run", AfterSequence: 1})
	if err != nil || len(events) != 1 || events[0].ID != "second" || events[0].Sequence != 2 {
		t.Fatalf("legacy activity stream = %#v, %v", events, err)
	}
	if _, err := service.ListActivity(context.Background(), models.ActivityFilter{Scope: scope}); err == nil || err.Error() != "run id is required" {
		t.Fatal("run stream accepted missing run id")
	}
}

func activityFeedRun(id string, scope models.Scope, agentID string, now time.Time) *models.AgentRun {
	return &models.AgentRun{
		ID: id, RootRunID: id, Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: agentID},
		AssignedAgentID: agentID, Goal: "work", Source: models.RunSourceManual, Status: models.AgentRunStatusQueued,
		AvailableAt: now, QueueEnteredAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
