package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestActivityFeedProjectsStableSummaryFirstPages(t *testing.T) {
	stores := []struct {
		name  string
		store KernelStore
		close func()
	}{
		{name: "memory", store: NewMemoryStore(), close: func() {}},
	}
	sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	stores = append(stores, struct {
		name  string
		store KernelStore
		close func()
	}{name: "sqlite", store: sqlite, close: func() { _ = sqlite.Close() }})

	for _, fixture := range stores {
		t.Run(fixture.name, func(t *testing.T) {
			defer fixture.close()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "one"}
			now := time.Date(2026, 7, 10, 7, 0, 0, 0, time.UTC)
			initiativeStore, ok := fixture.store.(InitiativeStore)
			if !ok {
				t.Fatal("activity feed store does not implement InitiativeStore")
			}
			if err := initiativeStore.CreateInitiative(ctx, &Initiative{
				ID: "initiative-one", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"},
				Title: "Research", Purpose: "Collect evidence", Status: InitiativeStatusActive, ObjectiveRefs: []string{"objective-one"},
				Revision: 1, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			for _, run := range []*AgentRun{
				activityFeedRun("run-one", scope, "agent-one", now),
				activityFeedRun("run-two", scope, "agent-one", now),
				activityFeedRun("run-other", scope, "agent-two", now),
			} {
				if err := fixture.store.CreateAgentRun(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			service := NewRunActivityService(fixture.store, fixture.store)
			for _, event := range []*ActivityEvent{
				{ID: "event-a", Scope: scope, RunID: "run-one", AgentID: "agent-one", InitiativeID: "initiative-one", EventType: "run.claimed", Summary: "Claimed", Actor: ActivityActor{Type: "worker", ID: "one"}, Visibility: ActivityVisibilityScope, CreatedAt: now, Payload: map[string]interface{}{"evidence": "artifact://one"}},
				{ID: "event-b", Scope: scope, RunID: "run-two", AgentID: "agent-one", EventType: "action.succeeded", Summary: "Published", Actor: ActivityActor{Type: "agent", ID: "agent-one"}, Visibility: ActivityVisibilityTeam, CreatedAt: now, CorrelationID: "correlation", UsageDelta: &BudgetUsage{Turns: 1, InputTokens: 120, OutputTokens: 30, CostMicros: 125_000, DurationMS: 500}},
				{ID: "event-c", Scope: scope, RunID: "run-other", AgentID: "agent-two", EventType: "run.completed", Summary: "Other", Actor: ActivityActor{Type: "agent", ID: "agent-two"}, Visibility: ActivityVisibilityScope, CreatedAt: now.Add(time.Second)},
				{ID: "event-private", Scope: scope, RunID: "run-one", AgentID: "agent-one", EventType: "turn.reasoned", Summary: "Private", Actor: ActivityActor{Type: "agent", ID: "agent-one"}, Visibility: ActivityVisibilityPrivate, CreatedAt: now.Add(2 * time.Second)},
			} {
				if _, err := service.AppendActivity(ctx, event); err != nil {
					t.Fatal(err)
				}
			}

			page, err := service.ListActivityFeed(ctx, ActivityFeedRequest{
				Scope: scope, AgentID: "agent-one", Limit: 1,
				Visibilities: []ActivityVisibility{ActivityVisibilityScope, ActivityVisibilityTeam},
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
			next, err := service.ListActivityFeed(ctx, ActivityFeedRequest{
				Scope: scope, AgentID: "agent-one", Cursor: page.NextCursor, Limit: 1, IncludeDetails: true,
				Visibilities: []ActivityVisibility{ActivityVisibilityScope, ActivityVisibilityTeam},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(next.Items) != 1 || next.Items[0].ID != "event-a" || next.Items[0].InitiativeID != "initiative-one" || next.Items[0].Payload["evidence"] != "artifact://one" || next.HasMore {
				t.Fatalf("detail page = %#v", next)
			}

			initiativePage, err := service.ListActivityFeed(ctx, ActivityFeedRequest{
				Scope: scope, InitiativeID: "initiative-one", Limit: 10, IncludeDetails: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(initiativePage.Items) != 1 || initiativePage.Items[0].ID != "event-a" || initiativePage.Items[0].Payload["evidence"] != "artifact://one" {
				t.Fatalf("initiative page = %#v", initiativePage)
			}
		})
	}
}

func TestActivityEventRejectsInvalidUsageDelta(t *testing.T) {
	event := &ActivityEvent{
		Scope: Scope{Kind: "tenant", ID: "one"}, RunID: "run", EventType: "turn.completed", Summary: "Completed",
		UsageDelta: &BudgetUsage{InputTokens: -1},
	}
	if err := event.Validate(); err == nil {
		t.Fatal("negative activity usage delta was accepted")
	}
}

func TestActivityFeedRequiresSelectorAndRejectsInvalidCursor(t *testing.T) {
	store := NewMemoryStore()
	service := NewRunActivityService(store, store)
	scope := Scope{Kind: "tenant", ID: "one"}
	if _, err := service.ListActivityFeed(context.Background(), ActivityFeedRequest{Scope: scope}); err == nil {
		t.Fatal("selector-free activity feed was accepted")
	}
	if _, err := service.ListActivityFeed(context.Background(), ActivityFeedRequest{Scope: scope, AgentID: "agent", Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("invalid activity cursor was accepted")
	}
	if _, err := service.ListActivityFeed(context.Background(), ActivityFeedRequest{Scope: scope, AgentID: "agent", Limit: maximumActivityFeedLimit + 1}); err == nil {
		t.Fatal("oversized activity page was accepted")
	}
}

func TestExistingRunActivityStreamKeepsSequenceCursor(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC()
	if err := store.CreateAgentRun(context.Background(), activityFeedRun("run", scope, "agent", now)); err != nil {
		t.Fatal(err)
	}
	service := NewRunActivityService(store, store)
	for _, id := range []string{"first", "second"} {
		if _, err := service.AppendActivity(context.Background(), &ActivityEvent{
			ID: id, Scope: scope, RunID: "run", AgentID: "agent", EventType: "run.updated", Summary: id,
			Actor: ActivityActor{Type: "agent", ID: "agent"}, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := service.ListActivity(context.Background(), ActivityFilter{Scope: scope, RunID: "run", AfterSequence: 1})
	if err != nil || len(events) != 1 || events[0].ID != "second" || events[0].Sequence != 2 {
		t.Fatalf("legacy activity stream = %#v, %v", events, err)
	}
	if _, err := service.ListActivity(context.Background(), ActivityFilter{Scope: scope}); err == nil || err.Error() != "run id is required" {
		t.Fatal("run stream accepted missing run id")
	}
}

func activityFeedRun(id string, scope Scope, agentID string, now time.Time) *AgentRun {
	return &AgentRun{
		ID: id, RootRunID: id, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID},
		AssignedAgentID: agentID, Goal: "work", Source: RunSourceManual, Status: AgentRunStatusQueued,
		AvailableAt: now, QueueEnteredAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
