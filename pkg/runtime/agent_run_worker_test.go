package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type countingAgentRunClaimStore struct {
	*sqlite.Store
	claims atomic.Int64
}

func (s *countingAgentRunClaimStore) ClaimNextAgentRunWithDecision(ctx context.Context, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
	s.claims.Add(1)
	return s.Store.ClaimNextAgentRunWithDecision(ctx, claim)
}

func TestAgentRunWorkerPoolCoalescesIdleClaimPolling(t *testing.T) {
	store := &countingAgentRunClaimStore{Store: newTestKernelStore(t)}
	pool, err := runworker.NewAgentRunWorkerPool(store, conversation.TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return nil, errors.New("unexpected resolver call")
	}), nil, runworker.AgentRunWorkerConfig{
		Scope: Scope{Kind: "tenant", ID: "idle"}, Concurrency: 8,
		PollInterval: 25 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(context.Background())
	time.Sleep(200 * time.Millisecond)
	pool.Stop()

	claims := store.claims.Load()
	if claims < 2 {
		t.Fatalf("expected periodic idle claim probes, got %d", claims)
	}
	if claims > 18 {
		t.Fatalf("idle claim probes scaled with worker concurrency: got %d", claims)
	}
}
