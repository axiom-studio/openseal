package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMemoryAgentRunClaimsAreAtomicAndRecoverExpiredLeases(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 20
	results := make(chan *AgentRun, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, claimErr := store.ClaimNextAgentRun(ctx, AgentRunClaim{
				Scope: scope, WorkerID: fmt.Sprintf("worker-%d", i), Now: now,
				LeaseDuration: time.Minute, AgingInterval: time.Minute,
			})
			results <- claimed
			errs <- claimErr
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	successes := 0
	var owner string
	for claimed := range results {
		if claimed != nil {
			successes++
			owner = claimed.LeaseOwner
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims = %d, want 1", successes)
	}
	if _, err := store.RenewAgentRunLease(ctx, scope, run.ID, "stale-worker", now.Add(10*time.Second), time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale renewal error = %v", err)
	}
	renewed, err := store.RenewAgentRunLease(ctx, scope, run.ID, owner, now.Add(10*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Revision != 3 || !renewed.LeaseExpiresAt.Equal(now.Add(70*time.Second)) {
		t.Fatalf("unexpected renewal: %#v", renewed)
	}
	recovered, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, WorkerID: "recovery-worker", Now: now.Add(71 * time.Second),
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.ID != run.ID || recovered.LeaseOwner != "recovery-worker" || recovered.Attempt != 2 {
		t.Fatalf("expired lease was not recovered: %#v", recovered)
	}
}

func TestMemoryAgentRunClaimHonorsCapacityAndScope(t *testing.T) {
	store := NewMemoryStore(100)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	otherScope := Scope{Kind: "tenant", ID: "two"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	service := NewPortfolioService(store)
	service.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		_, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
			Goal: fmt.Sprintf("work %d", i), Source: RunSourceObjective,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	other, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: otherScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "other work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := AgentRunClaim{Scope: scope, WorkerID: "worker-1", AssignedAgentID: "agent", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 1}
	first, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	claim.WorkerID = "worker-2"
	second, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatalf("capacity allowed a second live run: %#v", second)
	}
	otherClaim, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: otherScope, WorkerID: "worker-other", AssignedAgentID: "agent", Now: now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if otherClaim == nil || otherClaim.ID != other.ID {
		t.Fatalf("scope-isolated run was not independently claimable: %#v", otherClaim)
	}
}
