package workqueue_test

import (
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

func TestScopeWakeQueueDefersBusyScopes(t *testing.T) {
	queue := workqueue.NewScopeWakeQueue()
	now := time.Now()
	a, b := models.Scope{Kind: "tenant", ID: "a"}, models.Scope{Kind: "tenant", ID: "b"}
	queue.WakeScope(b)
	queue.WakeScope(a)
	ready, deferred := queue.Take(now, 0)
	if len(ready) != 2 || ready[0] != a || ready[1] != b || len(deferred) != 0 {
		t.Fatalf("take = %#v %#v", ready, deferred)
	}
	// A scope that just ran is deferred to the end of its spacing.
	queue.WakeScope(b)
	ready, deferred = queue.Take(now.Add(time.Second), 5*time.Second)
	if len(ready) != 0 || !deferred[b].Equal(now.Add(5*time.Second)) {
		t.Fatalf("busy scope = %#v %#v", ready, deferred)
	}
	queue.WakeScope(b)
	if ready, _ := queue.Take(now.Add(5*time.Second), 5*time.Second); len(ready) != 1 || ready[0] != b {
		t.Fatalf("deferred scope did not run at the end of its spacing: %#v", ready)
	}
	if retry := queue.Outcome(a, errors.New("down"), time.Second, now); retry == nil || !retry.After(now) {
		t.Fatalf("failure retry = %v", retry)
	}
	if retry := queue.Outcome(a, nil, time.Second, now); retry != nil {
		t.Fatalf("success scheduled a retry at %v", retry)
	}
}

func TestMemoryWorkQueueKeepsEarliestDueAndAsksForRebuild(t *testing.T) {
	queue := workqueue.NewMemoryWorkQueue()
	scope := models.Scope{Kind: "tenant", ID: "a"}
	now := time.Now()
	if err := queue.PublishWork(t.Context(), []models.WorkItem{
		{Family: models.WorkFamilyAction, Scope: scope, DueAt: now.Add(time.Hour)},
		{Family: models.WorkFamilyAction, Scope: scope, DueAt: now.Add(40 * time.Millisecond)},
		{Family: models.WorkFamilyAction, Scope: scope, DueAt: now.Add(time.Minute)},
		{Family: models.WorkFamilyRun, Scope: scope, DueAt: now.Add(-time.Second)},
		{Family: models.WorkFamilyRun, Scope: models.Scope{}, DueAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	signals := make(chan workqueue.WorkSignal, 8)
	go func() { _ = queue.ConsumeWork(t.Context(), func(signal workqueue.WorkSignal) { signals <- signal }) }()
	if connected := <-signals; connected.Kind != workqueue.WorkSignalConnected || !connected.Rebuild {
		t.Fatalf("first signal = %#v", connected)
	}
	if run := <-signals; run.Family != models.WorkFamilyRun || run.Scope != scope {
		t.Fatalf("overdue item = %#v", run)
	}
	select {
	case action := <-signals:
		if action.Family != models.WorkFamilyAction || time.Now().Before(now.Add(40*time.Millisecond)) {
			t.Fatalf("action item = %#v", action)
		}
	case <-time.After(time.Second):
		t.Fatal("earliest action deadline was not delivered")
	}
	select {
	case extra := <-signals:
		t.Fatalf("later duplicates were delivered: %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}
