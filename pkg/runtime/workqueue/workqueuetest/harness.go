// Package workqueuetest drives the workers that a work queue wakes: a memory
// queue bound to a signal status, as the Engine binds it, and a counting
// worker scope source.
package workqueuetest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

// Harness binds a status to a memory work queue, like the Engine does, and
// hands each due item to the delivery function under test.
type Harness struct {
	Status  *workqueue.WorkSignalStatus
	queue   *workqueue.MemoryWorkQueue
	deliver atomic.Pointer[func(workqueue.WorkSignal)]
}

// NewHarness starts a connected harness that stops when the test ends.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	h := &Harness{Status: workqueue.NewWorkSignalStatus(), queue: workqueue.NewMemoryWorkQueue()}
	publish := h.Status.BindQueue(h.queue, nil)
	go publish(t.Context())
	go func() {
		_ = h.queue.ConsumeWork(t.Context(), func(signal workqueue.WorkSignal) {
			h.Status.Observe(signal)
			if deliver := h.deliver.Load(); deliver != nil && signal.Kind == workqueue.WorkSignalChanged {
				(*deliver)(signal)
			}
		})
	}()
	WaitFor(t, time.Second, h.Status.Healthy, "queue did not connect")
	return h
}

// OnDelivery hands each due item to deliver.
func (h *Harness) OnDelivery(deliver func(workqueue.WorkSignal)) { h.deliver.Store(&deliver) }

// WaitFor polls condition every millisecond and fails the test with message
// when it does not hold within timeout.
func WaitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

// ScopeSource lists Scopes and answers point lookups with Active, counting
// both kinds of read.
type ScopeSource struct {
	Scopes atomic.Pointer[[]models.Scope]
	Lists  atomic.Int64
	Checks atomic.Int64
	Active atomic.Bool
}

func (s *ScopeSource) ListWorkerScopes(context.Context) ([]models.Scope, error) {
	s.Lists.Add(1)
	if scopes := s.Scopes.Load(); scopes != nil {
		return append([]models.Scope(nil), (*scopes)...), nil
	}
	return nil, nil
}

func (s *ScopeSource) WorkerScopeActive(context.Context, models.Scope) (bool, error) {
	s.Checks.Add(1)
	return s.Active.Load(), nil
}
