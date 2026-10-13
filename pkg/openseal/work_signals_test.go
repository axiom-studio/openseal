package openseal

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// signallingStore is an in-memory store whose work signals the test drives.
type signallingStore struct {
	*runtime.MemoryStore
	signals chan runtime.WorkSignal
	claims  atomic.Int64
}

func (s *signallingStore) ListenForWork(ctx context.Context, handle func(runtime.WorkSignal)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case signal := <-s.signals:
			handle(signal)
		}
	}
}

func (s *signallingStore) ClaimNextAction(ctx context.Context, claim runtime.ActionClaim) (*runtime.ActionCall, error) {
	s.claims.Add(1)
	return s.MemoryStore.ClaimNextAction(ctx, claim)
}

func (s *signallingStore) NextActionDeadline(context.Context, runtime.Scope, time.Time) (*time.Time, error) {
	return nil, nil
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEngineWorkSignalsWakeScopedWorkersAndCatchUpOnReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := Scope{Kind: "local", ID: "signals"}
	store := &signallingStore{MemoryStore: runtime.NewMemoryStore(), signals: make(chan runtime.WorkSignal)}
	engine, err := New(
		WithStore(store),
		WithActionWorkers(ActionWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Hour, LeaseDuration: time.Second}, nil,
			ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{}, nil
			})),
	)
	if err != nil {
		t.Fatal(err)
	}
	engine.Start(ctx)
	defer engine.Stop()
	if engine.WorkSignalHealth().Healthy {
		t.Fatal("listener reported healthy before connecting")
	}
	store.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalConnected}
	waitFor(t, func() bool { return engine.WorkSignalHealth().Healthy }, "connected listener was not reported healthy")
	waitFor(t, func() bool { return store.claims.Load() >= 1 }, "initial claim was not observed")
	time.Sleep(20 * time.Millisecond)

	claims := store.claims.Load()
	store.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalChanged, Family: runtime.WorkFamilyAction, Scope: Scope{Kind: "local", ID: "elsewhere"}}
	store.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalChanged, Family: runtime.WorkFamilyRun, Scope: scope}
	time.Sleep(50 * time.Millisecond)
	if got := store.claims.Load(); got != claims {
		t.Fatalf("unrelated signals caused %d action claims", got-claims)
	}
	store.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalChanged, Family: runtime.WorkFamilyAction, Scope: scope}
	waitFor(t, func() bool { return store.claims.Load() > claims }, "action signal did not wake the scope's pool")

	store.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalDisconnected}
	waitFor(t, func() bool { return !engine.WorkSignalHealth().Healthy }, "disconnect was not reported")
	time.Sleep(20 * time.Millisecond)
	claims = store.claims.Load()
	// Work committed while disconnected is found by the reconnect catch-up.
	store.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalConnected}
	waitFor(t, func() bool { return store.claims.Load() > claims }, "reconnect did not catch up")
	if health := engine.WorkSignalHealth(); !health.Healthy || health.Connects != 2 || health.Disconnects != 1 || health.Signals != 3 {
		t.Fatalf("health = %#v", health)
	}
}
