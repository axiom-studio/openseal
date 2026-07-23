package client

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestKernelHTTPClientOperatesEventSourceSubscriptions(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	api := server.NewServer(nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	ctx := context.Background()
	scope := runtime.Scope{Kind: "tenant", ID: "operations"}
	created, err := client.CreateEventSourceSubscription(ctx, kernelapi.CreateEventSourceSubscriptionRequest{
		ID: "cluster-events", Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "sre"},
		DisplayName: "Cluster events", Source: "kubernetes:cluster:production", Status: runtime.EventSourceSubscriptionActive,
		Connector: runtime.EventSourceConnector{Kind: runtime.EventSourceConnectorHost, ID: "kubernetes.watch", Version: "1"}, EventTypes: []string{"kubernetes.warning"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reported, err := client.ReportEventSourceHealth(ctx, scope, created.ID, kernelapi.ReportEventSourceHealthRequest{ObservedSubscriptionRevision: created.Revision, State: runtime.EventSourceHealthHealthy})
	if err != nil || reported.Revision != 1 {
		t.Fatalf("health = %#v, %v", reported, err)
	}
	checkpoint, err := client.AdvanceEventSourceCheckpoint(ctx, scope, created.ID, kernelapi.AdvanceEventSourceCheckpointRequest{ObservedSubscriptionRevision: created.Revision, Cursor: "cursor-1", EventIDs: []string{"event-1"}})
	if err != nil || checkpoint.Revision != 1 {
		t.Fatalf("checkpoint = %#v, %v", checkpoint, err)
	}
	loadedCheckpoint, err := client.GetEventSourceCheckpoint(ctx, scope, created.ID)
	if err != nil || loadedCheckpoint.Cursor != checkpoint.Cursor {
		t.Fatalf("loaded checkpoint = %#v, %v", loadedCheckpoint, err)
	}
	detail, err := client.GetEventSourceSubscription(ctx, scope, created.ID)
	if err != nil || detail.Health == nil || detail.Checkpoint == nil {
		t.Fatalf("detail = %#v, %v", detail, err)
	}
	if _, err := client.AdvanceEventSourceCheckpoint(ctx, scope, created.ID, kernelapi.AdvanceEventSourceCheckpointRequest{ObservedSubscriptionRevision: created.Revision + 1, ExpectedRevision: checkpoint.Revision, EventIDs: []string{"event-2"}}); err == nil {
		t.Fatal("stale connector advanced the checkpoint")
	}
	items, err := client.ListEventSourceSubscriptions(ctx, runtime.EventSourceSubscriptionFilter{Scope: scope, Statuses: []runtime.EventSourceSubscriptionStatus{runtime.EventSourceSubscriptionActive}})
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %#v, %v", items, err)
	}
	retired, err := client.RetireEventSourceSubscription(ctx, scope, created.ID, kernelapi.RetireEventSourceSubscriptionRequest{ExpectedRevision: created.Revision})
	if err != nil || retired.Status != runtime.EventSourceSubscriptionRetired {
		t.Fatalf("retired = %#v, %v", retired, err)
	}
}
