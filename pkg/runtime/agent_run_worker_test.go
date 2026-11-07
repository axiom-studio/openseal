package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type countingAgentRunClaimStore struct {
	*MemoryStore
	claims atomic.Int64
}

func (s *countingAgentRunClaimStore) ClaimNextAgentRunWithDecision(ctx context.Context, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
	s.claims.Add(1)
	return s.MemoryStore.ClaimNextAgentRunWithDecision(ctx, claim)
}

func TestAgentRunWorkerPoolCoalescesIdleClaimPolling(t *testing.T) {
	store := &countingAgentRunClaimStore{MemoryStore: NewMemoryStore()}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, errors.New("unexpected resolver call")
	}), nil, AgentRunWorkerConfig{
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

func TestDelegatedRunbookOriginPreservesImmutableIdentity(t *testing.T) {
	origin := delegatedRunbookOrigin(map[string]interface{}{
		"runbookDefinitionId": "hourly-scan", "runbookDefinitionVersion": "2.0.2",
		"runbookEntrypoint": "perform-hourly", "runbookActivationId": "activation-one", "runbookTriggerId": "hourly",
	})
	if origin["runbookDefinitionId"] != "hourly-scan" || origin["runbookDefinitionVersion"] != "2.0.2" ||
		origin["runbookEntrypoint"] != "perform-hourly" || origin["runbookActivationId"] != "activation-one" ||
		origin["runbookTriggerId"] != "hourly" {
		t.Fatalf("delegated Runbook origin = %#v", origin)
	}
	if delegatedRunbookOrigin(map[string]interface{}{"runbookDefinitionId": "hourly-scan"}) != nil {
		t.Fatal("incomplete Runbook origin was propagated")
	}
}
