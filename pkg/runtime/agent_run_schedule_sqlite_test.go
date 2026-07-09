package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLiteAgentRunClaimsAreAtomicAndRecoverAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-runs.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
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
	const contenders = 16
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
	for claimed := range results {
		if claimed != nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims = %d, want 1", successes)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, WorkerID: "recovery-worker", Now: now.Add(time.Minute),
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.ID != run.ID || recovered.Attempt != 2 || recovered.LeaseOwner != "recovery-worker" {
		t.Fatalf("run was not recovered after restart: %#v", recovered)
	}
}

func TestSQLiteAgentRunClaimHonorsAgingCapacityAndScope(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "portfolio-schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	otherScope := Scope{Kind: "tenant", ID: "two"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now.Add(-10 * time.Minute) }
	old, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "old low priority", Source: RunSourceObjective, Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	portfolio.now = func() time.Time { return now.Add(-time.Minute) }
	_, err = portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "new high priority", Source: RunSourceObjective, Priority: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: otherScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "other scope", Source: RunSourceObjective, Priority: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := AgentRunClaim{
		Scope: scope, WorkerID: "worker-1", AssignedAgentID: "agent", Now: now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 1,
	}
	first, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.ID != old.ID {
		t.Fatalf("aging did not select the starved run: %#v", first)
	}
	claim.WorkerID = "worker-2"
	second, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatalf("per-agent capacity was exceeded: %#v", second)
	}
	otherClaim, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: otherScope, WorkerID: "worker-other", Now: now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if otherClaim == nil || otherClaim.ID != other.ID {
		t.Fatalf("scope-isolated claim failed: %#v", otherClaim)
	}
}
