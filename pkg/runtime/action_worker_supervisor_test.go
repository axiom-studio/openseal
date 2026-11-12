package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

// mutableActionWorkerScopeSource reports a listing on listed once it has
// answered it, dropping the report while an earlier one is unread.
type mutableActionWorkerScopeSource struct {
	mu     sync.Mutex
	scopes []Scope
	err    error
	listed chan struct{}
}

func (s *mutableActionWorkerScopeSource) ListWorkerScopes(context.Context) ([]Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		select {
		case s.listed <- struct{}{}:
		default:
		}
	}()
	return append([]Scope(nil), s.scopes...), s.err
}

func (s *mutableActionWorkerScopeSource) set(scopes []Scope, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = append([]Scope(nil), scopes...)
	s.err = err
}

func TestActionWorkerSupervisorReconcilesIsolatedScopes(t *testing.T) {
	store := newTestKernelStore(t)
	source := &mutableActionWorkerScopeSource{scopes: []Scope{
		{Kind: "tenant", ID: "2"}, {Kind: "tenant", ID: "1"}, {Kind: "tenant", ID: "1"},
	}, listed: make(chan struct{}, 1)}
	supervisor, err := NewActionWorkerSupervisor(
		store, skill.NewCatalog(), nil,
		ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) { return nil, nil }),
		source, zap.NewNop().Sugar(), DynamicActionWorkerConfig{ReconcileInterval: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Without a work queue the supervisor lists the scopes when it starts
	// and on every refresh; the hour-long interval never lists on its own.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	<-source.listed
	if got := waitActionWorkerScopeCount(supervisor, 2); got != 2 {
		t.Fatalf("expected two deduplicated scope pools, got %d", got)
	}

	source.set([]Scope{{Kind: "tenant", ID: "2"}, {Kind: "", ID: "invalid"}}, nil)
	supervisor.RefreshScopes()
	<-source.listed
	if got := waitActionWorkerScopeCount(supervisor, 1); got != 1 {
		t.Fatalf("expected removed and invalid scopes to be absent, got %d pools", got)
	}

	source.set(nil, errors.New("temporary database failure"))
	supervisor.RefreshScopes()
	<-source.listed
	if got := supervisor.ScopeCount(); got != 1 {
		t.Fatalf("source failure must preserve existing pools, got %d", got)
	}
	supervisor.Stop()
}

// waitActionWorkerScopeCount waits up to a second for the supervisor to run
// want scope pools, and returns the count it saw last.
func waitActionWorkerScopeCount(supervisor *ActionWorkerSupervisor, want int) int {
	deadline := time.Now().Add(time.Second)
	for supervisor.ScopeCount() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return supervisor.ScopeCount()
}

func TestActionWorkerSupervisorStartStopIsIdempotent(t *testing.T) {
	supervisor, err := NewActionWorkerSupervisor(
		newTestKernelStore(t), skill.NewCatalog(), nil,
		ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) { return nil, nil }),
		workqueue.WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{{Kind: "tenant", ID: "1"}}, nil }),
		zap.NewNop().Sugar(), DynamicActionWorkerConfig{ReconcileInterval: 5 * time.Millisecond},
	)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Start(context.Background())
	supervisor.Start(context.Background())
	deadline := time.Now().Add(time.Second)
	for supervisor.ScopeCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := supervisor.ScopeCount(); got != 1 {
		t.Fatalf("expected one active scope, got %d", got)
	}
	supervisor.Stop()
	supervisor.Stop()
	if got := supervisor.ScopeCount(); got != 0 {
		t.Fatalf("expected all pools stopped, got %d", got)
	}
}
