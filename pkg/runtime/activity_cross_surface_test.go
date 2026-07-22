package runtime

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCanonicalActivityProjectionIsIdenticalAcrossResourceAndChannelSurfaces(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) {
			return NewMemoryStore(50), func() {}
		}},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "cross-surface-activity.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}

	for _, fixture := range stores {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			store, closeStore := fixture.open(t)
			defer closeStore()
			conversationStore, ok := store.(ConversationStore)
			if !ok {
				t.Fatal("kernel store does not implement ConversationStore")
			}

			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "cross-surface"}
			now := time.Date(2026, 7, 23, 8, 30, 0, 0, time.UTC)
			conversation, _, err := NewConversationService(conversationStore).CreateConversation(ctx, CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-gtm"},
				Title: "Launch coordination", IdempotencyKey: "launch-coordination",
			})
			if err != nil {
				t.Fatal(err)
			}
			objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: conversation.Owner, Title: "Launch", Goal: "Publish the approved launch",
				Status: ObjectiveStatusActive, IdempotencyKey: "objective-launch",
			})
			if err != nil {
				t.Fatal(err)
			}
			initiativeStore, ok := store.(InitiativeStore)
			if !ok {
				t.Fatal("kernel store does not implement InitiativeStore")
			}
			initiative := &Initiative{
				ID: "initiative-launch", Scope: scope, Owner: conversation.Owner, Title: "Launch initiative",
				Purpose: "Coordinate the product launch", Status: InitiativeStatusActive, ObjectiveRefs: []string{objective.ID},
				Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := initiativeStore.CreateInitiative(ctx, initiative); err != nil {
				t.Fatal(err)
			}
			run := activityFeedRun("run-launch", scope, "agent-marketing", now)
			run.Kind = RunKindConversation
			run.Owner = conversation.Owner
			run.ObjectiveID = objective.ID
			run.ConcurrencyKey = conversation.ID
			if err := store.CreateAgentRun(ctx, run); err != nil {
				t.Fatal(err)
			}

			activity := NewRunActivityService(store, store)
			appended, err := activity.AppendActivity(ctx, &ActivityEvent{
				ID: "event-launch-published", Scope: scope, EventType: "action.succeeded",
				AgentID: "agent-marketing", ObjectiveID: objective.ID, InitiativeID: initiative.ID,
				RunID: run.ID, TurnID: "turn-publish", ParentRunID: "run-development", TeamID: "team-gtm",
				ConversationRefs: []string{conversation.ID}, Actor: ActivityActor{Type: "agent", ID: "agent-marketing"},
				Summary: "Published the approved launch brief", Visibility: ActivityVisibilityTeam,
				UsageDelta:    &BudgetUsage{Turns: 1, InputTokens: 800, OutputTokens: 120, CostMicros: 42_000, DurationMS: 1_500, Actions: 1},
				Payload:       map[string]interface{}{"artifactRef": "artifact-launch-brief", "secret": "must-not-project"},
				CorrelationID: "correlation-launch", CausationID: "event-development-completed", CreatedAt: now,
			})
			if err != nil {
				t.Fatal(err)
			}

			selectors := map[string]ActivityFeedRequest{
				"agent":      {Scope: scope, AgentID: appended.AgentID},
				"team":       {Scope: scope, TeamID: appended.TeamID},
				"objective":  {Scope: scope, ObjectiveID: appended.ObjectiveID},
				"initiative": {Scope: scope, InitiativeID: appended.InitiativeID},
				"run":        {Scope: scope, RunID: appended.RunID},
			}
			var canonical *ActivityProjection
			for name, request := range selectors {
				request.EventTypes = []string{appended.EventType}
				page, listErr := activity.ListActivityFeed(ctx, request)
				if listErr != nil {
					t.Fatalf("%s feed: %v", name, listErr)
				}
				if len(page.Items) != 1 {
					t.Fatalf("%s feed items = %#v", name, page.Items)
				}
				projection := page.Items[0]
				if canonical == nil {
					canonical = &projection
					continue
				}
				if !reflect.DeepEqual(*canonical, projection) {
					t.Fatalf("%s projection differs\ncanonical: %#v\nactual:    %#v", name, *canonical, projection)
				}
			}

			changes, err := NewConversationChangeService(conversationStore, store)
			if err != nil {
				t.Fatal(err)
			}
			channel, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, ActiveAt: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(channel.Activity) != 1 || !reflect.DeepEqual(*canonical, channel.Activity[0]) {
				t.Fatalf("channel projection differs\ncanonical: %#v\nchannel:   %#v", *canonical, channel.Activity)
			}
			if canonical.Payload != nil || len(canonical.ConversationRefs) != 0 || canonical.CorrelationID != "" || canonical.CausationID != "" ||
				!canonical.DetailAvailable || canonical.UsageDelta == nil || canonical.UsageDelta.CostMicros != 42_000 {
				t.Fatalf("canonical summary projection leaked detail or lost bounded facts: %#v", *canonical)
			}
		})
	}
}
