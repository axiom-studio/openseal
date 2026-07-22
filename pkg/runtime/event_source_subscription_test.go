package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestEventSourceSubscriptionLifecycleHealthAndCheckpointSurviveRestart(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T) (KernelStore, func() KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func() KernelStore, func()) {
			store := NewMemoryStore(100)
			return store, func() KernelStore { return store }, func() {}
		}},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func() KernelStore, func()) {
			path := filepath.Join(t.TempDir(), "kernel.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			current := store
			return store, func() KernelStore {
				if err := current.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				current = reopened
				return reopened
			}, func() { _ = current.Close() }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, restart, closeStore := test.open(t)
			defer closeStore()
			service := NewEventSourceSubscriptionService(store, store)
			scope := Scope{Kind: "tenant", ID: "operations"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"}
			subscription, err := service.Create(ctx, CreateEventSourceSubscriptionRequest{
				ID: "production-warnings", Scope: scope, Owner: owner, DisplayName: "Production warnings",
				Description: "Watch normalized Kubernetes warning events", Source: "kubernetes:cluster:production",
				Connector:  EventSourceConnector{Kind: EventSourceConnectorHost, ID: "kubernetes.watch", Version: "1"},
				EventTypes: []string{"kubernetes.warning"},
				Parameters: map[string]interface{}{"namespace": "production", "reasons": []interface{}{"BackOff"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if subscription.Status != EventSourceSubscriptionPaused || subscription.Revision != 1 {
				t.Fatalf("subscription = %#v", subscription)
			}

			active := EventSourceSubscriptionActive
			subscription, err = service.Update(ctx, scope, subscription.ID, UpdateEventSourceSubscriptionRequest{ExpectedRevision: 1, Status: &active})
			if err != nil || subscription.Status != active || subscription.Revision != 2 {
				t.Fatalf("active = %#v, %v", subscription, err)
			}
			health, err := service.ReportHealth(ctx, ReportEventSourceHealthRequest{
				Scope: scope, SubscriptionID: subscription.ID, ObservedSubscriptionRevision: subscription.Revision,
				State: EventSourceHealthDegraded, ErrorCode: "upstream-timeout", Summary: "The upstream watch timed out",
			})
			if err != nil || health.Revision != 1 || health.ConsecutiveFailures != 1 || health.State != EventSourceHealthDegraded {
				t.Fatalf("health = %#v, %v", health, err)
			}
			health, err = service.ReportHealth(ctx, ReportEventSourceHealthRequest{
				Scope: scope, SubscriptionID: subscription.ID, ExpectedHealthRevision: health.Revision,
				ObservedSubscriptionRevision: subscription.Revision, State: EventSourceHealthHealthy, LastEventAt: time.Now().UTC(),
			})
			if err != nil || health.Revision != 2 || health.ConsecutiveFailures != 0 || health.LastSuccessAt.IsZero() || health.ErrorCode != "" {
				t.Fatalf("recovered health = %#v, %v", health, err)
			}
			checkpoint, err := NewEventSourceCheckpointService(store).Advance(ctx, AdvanceEventSourceCheckpointRequest{
				Scope: scope, Source: subscription.Source, SubscriptionID: subscription.ID, Cursor: "resource-version-10", EventIDs: []string{"event-1"},
			})
			if err != nil || checkpoint.Revision != 1 {
				t.Fatalf("checkpoint = %#v, %v", checkpoint, err)
			}

			store = restart()
			service = NewEventSourceSubscriptionService(store, store)
			detail, err := service.Get(ctx, scope, subscription.ID)
			if err != nil || detail.Subscription.Revision != 2 || detail.Health.Revision != 2 || detail.Checkpoint.Cursor != "resource-version-10" {
				t.Fatalf("restored detail = %#v, %v", detail, err)
			}
			items, err := service.List(ctx, EventSourceSubscriptionFilter{Scope: scope, Owner: &owner, Statuses: []EventSourceSubscriptionStatus{EventSourceSubscriptionActive}, ConnectorKind: EventSourceConnectorHost})
			if err != nil || len(items) != 1 || items[0].ID != subscription.ID {
				t.Fatalf("items = %#v, %v", items, err)
			}

			poll := int64(30)
			subscription, err = service.Update(ctx, scope, subscription.ID, UpdateEventSourceSubscriptionRequest{ExpectedRevision: 2, PollIntervalSeconds: &poll})
			if err != nil || subscription.Revision != 3 {
				t.Fatalf("updated = %#v, %v", subscription, err)
			}
			_, err = service.ReportHealth(ctx, ReportEventSourceHealthRequest{
				Scope: scope, SubscriptionID: subscription.ID, ExpectedHealthRevision: 2, ObservedSubscriptionRevision: 2, State: EventSourceHealthHealthy,
			})
			if !errors.Is(err, ErrEventSourceSubscriptionConflict) {
				t.Fatalf("stale connector health = %v", err)
			}
			retired := EventSourceSubscriptionRetired
			subscription, err = service.Update(ctx, scope, subscription.ID, UpdateEventSourceSubscriptionRequest{ExpectedRevision: 3, Status: &retired})
			if err != nil || subscription.Status != retired {
				t.Fatalf("retired = %#v, %v", subscription, err)
			}
			_, err = service.Update(ctx, scope, subscription.ID, UpdateEventSourceSubscriptionRequest{ExpectedRevision: 4, Status: &active})
			if !errors.Is(err, ErrInvalidEventSourceSubscription) {
				t.Fatalf("reactivate retired = %v", err)
			}
		})
	}
}

func TestEventSourceSubscriptionRejectsCredentialLeakageAndInvalidSkillConnector(t *testing.T) {
	service := NewEventSourceSubscriptionService(NewMemoryStore(20), NewMemoryStore(20))
	base := CreateEventSourceSubscriptionRequest{
		Scope: Scope{Kind: "tenant", ID: "research"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"},
		DisplayName: "Forum monitor", Source: "forum:example", EventTypes: []string{"forum.post"},
		Connector: EventSourceConnector{Kind: EventSourceConnectorSkill, ID: "forum.reader", Version: "1", BindingID: "forum-binding", Action: "watch"},
	}
	unsafe := base
	unsafe.Parameters = map[string]interface{}{"apiToken": "must-not-persist"}
	if _, err := service.Create(t.Context(), unsafe); !errors.Is(err, ErrInvalidEventSourceSubscription) {
		t.Fatalf("credential-shaped parameters = %v", err)
	}
	missingBinding := base
	missingBinding.Connector.BindingID = ""
	if _, err := service.Create(t.Context(), missingBinding); !errors.Is(err, ErrInvalidEventSourceSubscription) {
		t.Fatalf("missing governed binding = %v", err)
	}
}
