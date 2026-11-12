package conversationrun_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue/workqueuetest"
)

type signalConversationStore struct {
	*sqlite.Store
	conversationLists atomic.Int64
}

func (s *signalConversationStore) ListConversations(ctx context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	s.conversationLists.Add(1)
	return s.Store.ListConversations(ctx, filter)
}

func TestQueueDrivenConversationRunReconcilerRunsOnlyQueuedScopes(t *testing.T) {
	store := &signalConversationStore{Store: kerneltest.NewStore(t)}
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scope, other := models.Scope{Kind: "tenant", ID: "a"}, models.Scope{Kind: "tenant", ID: "b"}
	source := &workqueuetest.ScopeSource{}
	source.Active.Store(true)
	source.Scopes.Store(&[]models.Scope{scope, other})
	reconciler, err := conversationrun.NewConversationRunReconciler(scheduler, source, nil, conversationrun.ConversationRunReconcilerConfig{Interval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	harness := workqueuetest.NewHarness(t)
	harness.OnDelivery(func(signal workqueue.WorkSignal) {
		if signal.Family == models.WorkFamilyConversationDue {
			reconciler.WakeScope(signal.Scope)
		}
	})
	reconciler.SetWorkSignals(harness.Status)
	reconciler.Start(t.Context())
	defer reconciler.Stop()
	// With a queue there is no full pass, at start or on the tick.
	time.Sleep(350 * time.Millisecond)
	if lists, scopes := store.conversationLists.Load(), source.Lists.Load(); lists != 0 || scopes != 0 {
		t.Fatalf("idle queue-driven reconciler read %d conversation pages and %d scope lists", lists, scopes)
	}
	reconciler.WakeScope(scope)
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.conversationLists.Load() == 1 }, "scope wake did not reconcile")
	// A second wake within the spacing is deferred through the queue.
	reconciler.WakeScope(scope)
	time.Sleep(30 * time.Millisecond)
	if lists := store.conversationLists.Load(); lists != 1 {
		t.Fatalf("busy scope reconciled again within its spacing: %d", lists)
	}
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.conversationLists.Load() == 2 }, "deferred pass was not delivered")
	if scopes := source.Lists.Load(); scopes != 0 {
		t.Fatalf("queued passes listed scopes %d times", scopes)
	}
	// Without a healthy queue the configured tick reconciles every scope.
	harness.Status.Observe(workqueue.WorkSignal{Kind: workqueue.WorkSignalDisconnected})
	workqueuetest.WaitFor(t, time.Second, func() bool { return source.Lists.Load() >= 2 }, "unhealthy queue did not restore the tick")
}
