package portfolio_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestAgentRunClaimsAreAtomicAndRecoverExpiredLeases(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "test"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "work", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 20
	results := make(chan *models.AgentRun, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, claimErr := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
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
	if _, err := store.RenewAgentRunLease(ctx, scope, run.ID, "stale-worker", now.Add(10*time.Second), time.Minute); !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("stale renewal error = %v", err)
	}
	renewed, err := store.RenewAgentRunLease(ctx, scope, run.ID, owner, now.Add(10*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Revision != 3 || !renewed.LeaseExpiresAt.Equal(now.Add(70*time.Second)) {
		t.Fatalf("unexpected renewal: %#v", renewed)
	}
	recovered, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
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

func TestAgentRunAttemptBudgetSurvivesLeaseRecovery(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "attempt-budget"}
	now := time.Now().UTC()
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "recover at most once", Source: models.RunSourceObjective, Budget: &models.BudgetPolicy{MaxAttempts: 1, MaxTurns: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "first", Now: now, LeaseDuration: time.Second, AgingInterval: time.Minute})
	if err != nil || first == nil || first.BudgetUsage.Attempts != 1 || first.BudgetState != models.BudgetStateExhausted {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	recovered, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "recovery", Now: now.Add(2 * time.Second), LeaseDuration: time.Second, AgingInterval: time.Minute})
	if err != nil || recovered == nil || recovered.ID != run.ID || recovered.Status != models.AgentRunStatusPaused ||
		recovered.BudgetUsage.Attempts != 1 || !models.RunAttemptBudgetAtLimit(recovered) || models.RunAttemptBudgetExceeded(recovered) {
		t.Fatalf("recovered claim = %#v, %v", recovered, err)
	}
}

func TestAgentRunClaimHonorsCapacityAndScope(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	otherScope := models.Scope{Kind: "tenant", ID: "two"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	service := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	for i := 0; i < 2; i++ {
		_, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
			Goal: fmt.Sprintf("work %d", i), Source: models.RunSourceObjective,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	other, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: otherScope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "other work", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := models.AgentRunClaim{Scope: scope, WorkerID: "worker-1", AssignedAgentID: "agent", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 1}
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
	otherClaim, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
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

func TestAgentRunClaimHonorsConcurrencyKeyCapacity(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "conversation-capacity"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	for _, candidate := range []struct {
		key      string
		priority int
	}{
		{key: "channel-a", priority: 10},
		{key: "channel-a", priority: 9},
		{key: "channel-b", priority: 1},
	} {
		if _, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"},
			ConcurrencyKey: candidate.key, Goal: "Coordinate " + candidate.key, Source: models.RunSourceChat, Priority: candidate.priority,
		}); err != nil {
			t.Fatal(err)
		}
	}
	claim := models.AgentRunClaim{
		Scope: scope, Kind: models.RunKindConversation, WorkerID: "worker-1", Now: now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForConcurrencyKey: 1,
	}
	first, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil || first == nil || first.ConcurrencyKey != "channel-a" {
		t.Fatalf("first keyed claim = %#v, %v", first, err)
	}
	claim.WorkerID = "worker-2"
	second, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil || second == nil || second.ConcurrencyKey != "channel-b" {
		t.Fatalf("independent keyed claim = %#v, %v", second, err)
	}
	claim.WorkerID = "worker-3"
	blocked, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if blocked != nil {
		t.Fatalf("same-key capacity allowed overlapping claim: %#v", blocked)
	}
	claim.WorkerID = "recovery-worker"
	claim.Now = now.Add(time.Minute)
	recovered, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil || recovered == nil || recovered.ConcurrencyKey != "channel-a" {
		t.Fatalf("expired keyed lease was not recoverable: %#v, %v", recovered, err)
	}
}

func TestAgentRunClaimIsolatesRunKinds(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "kinds"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	agentRun, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "ordinary work",
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationRun, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"},
		Goal: "coordinate a channel", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimedConversation, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: scope, Kind: models.RunKindConversation, WorkerID: "conversation-worker", Now: now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimedConversation == nil || claimedConversation.ID != conversationRun.ID {
		t.Fatalf("conversation claim = %#v, %v", claimedConversation, err)
	}
	claimedAgent, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: scope, Kind: models.RunKindAgentWork, WorkerID: "agent-worker", Now: now,
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimedAgent == nil || claimedAgent.ID != agentRun.ID || claimedAgent.Kind != models.RunKindAgentWork {
		t.Fatalf("agent claim = %#v, %v", claimedAgent, err)
	}
}
