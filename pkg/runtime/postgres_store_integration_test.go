//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresExecutionStoreConformanceAndReplicaClaims(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	schema := "openseal_test_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	runExecutionStoreConformance(t, primary)

	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	const runCount = 24
	for index := 0; index < runCount; index++ {
		if _, err := primary.CreateRun(ctx, testWorkflow(0), map[string]interface{}{"index": index}); err != nil {
			t.Fatal(err)
		}
	}
	stores := []*PostgresStore{primary, replica}
	claimed := make(chan int, runCount)
	claimErrors := make(chan error, 8)
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			store := stores[worker%len(stores)]
			for {
				run, claimErr := store.ClaimNextRunnable(ctx, "worker-"+string(rune('a'+worker)), time.Minute)
				if claimErr != nil {
					claimErrors <- claimErr
					return
				}
				if run == nil {
					return
				}
				claimed <- run.RunID
			}
		}(worker)
	}
	wait.Wait()
	close(claimed)
	close(claimErrors)
	for claimErr := range claimErrors {
		t.Fatal(claimErr)
	}
	seen := make(map[int]bool)
	for runID := range claimed {
		if seen[runID] {
			t.Fatalf("run %d was claimed more than once", runID)
		}
		seen[runID] = true
	}
	if len(seen) != runCount {
		t.Fatalf("unique claims = %d, want %d", len(seen), runCount)
	}

	retryID, err := primary.CreateRun(ctx, testWorkflow(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.ClaimNextRunnable(ctx, "retry-owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(80 * time.Millisecond)
	if err := primary.ScheduleRetry(ctx, retryID, "retry-owner", due, errors.New("transient")); err != nil {
		t.Fatal(err)
	}
	if early, err := replica.ClaimNextRunnable(ctx, "retry-replica", time.Minute); err != nil || early != nil {
		t.Fatalf("early retry claim = %#v, %v", early, err)
	}
	time.Sleep(time.Until(due) + 20*time.Millisecond)
	recovered, err := replica.ClaimNextRunnable(ctx, "retry-replica", time.Minute)
	if err != nil || recovered == nil || recovered.RunID != retryID || recovered.RetryCount != 1 {
		t.Fatalf("recovered retry = %#v, %v", recovered, err)
	}

	var migrationCount int
	if err := primary.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+primary.table("schema_migrations")+` WHERE version = 1`).Scan(&migrationCount); err != nil || migrationCount != 1 {
		t.Fatalf("migration count = %d, %v", migrationCount, err)
	}

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(primary)
	portfolio.now = func() time.Time { return now }
	scope := Scope{Kind: "tenant", ID: "postgres-e2e"}
	otherScope := Scope{Kind: "tenant", ID: "other"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"}
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Operate", Goal: "Keep services healthy", Status: ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if crossScope, err := primary.GetObjective(ctx, otherScope, objective.ID); err != nil || crossScope != nil {
		t.Fatalf("cross-scope objective = %#v, %v", crossScope, err)
	}
	summary := "Healthy"
	updated, err := portfolio.UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{ExpectedRevision: objective.Revision, ProgressSummary: &summary})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("updated objective = %#v, %v", updated, err)
	}
	if _, err := portfolio.UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{ExpectedRevision: objective.Revision, ProgressSummary: &summary}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale objective update = %v", err)
	}

	const agentRunCount = 16
	agentRunIDs := make(map[string]bool, agentRunCount)
	for index := 0; index < agentRunCount; index++ {
		run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: "operator", Goal: "Inspect shard", Source: RunSourceObjective, Priority: index % 3})
		if err != nil {
			t.Fatal(err)
		}
		agentRunIDs[run.ID] = true
	}
	agentClaims := make(chan string, agentRunCount)
	agentClaimErrors := make(chan error, 8)
	wait = sync.WaitGroup{}
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			store := stores[worker%len(stores)]
			for {
				run, claimErr := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "agent-worker-" + string(rune('a'+worker)), Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
				if claimErr != nil {
					agentClaimErrors <- claimErr
					return
				}
				if run == nil {
					return
				}
				agentClaims <- run.ID
			}
		}(worker)
	}
	wait.Wait()
	close(agentClaims)
	close(agentClaimErrors)
	for claimErr := range agentClaimErrors {
		t.Fatal(claimErr)
	}
	claimedAgentRuns := make(map[string]bool)
	for runID := range agentClaims {
		if claimedAgentRuns[runID] || !agentRunIDs[runID] {
			t.Fatalf("invalid duplicate agent run claim %q", runID)
		}
		claimedAgentRuns[runID] = true
	}
	if len(claimedAgentRuns) != agentRunCount {
		t.Fatalf("unique agent run claims = %d, want %d", len(claimedAgentRuns), agentRunCount)
	}

	for index := 0; index < 2; index++ {
		if _, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "limited"}, AssignedAgentID: "limited", Goal: "Capacity limited work", Source: RunSourceObjective}); err != nil {
			t.Fatal(err)
		}
	}
	limited := make(chan *AgentRun, 2)
	limitedErrors := make(chan error, 2)
	wait = sync.WaitGroup{}
	for worker := 0; worker < 2; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			run, claimErr := stores[worker].ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "limited-worker-" + string(rune('a'+worker)), AssignedAgentID: "limited", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 1})
			if claimErr != nil {
				limitedErrors <- claimErr
				return
			}
			limited <- run
		}(worker)
	}
	wait.Wait()
	close(limited)
	close(limitedErrors)
	for claimErr := range limitedErrors {
		t.Fatal(claimErr)
	}
	limitedClaims := 0
	for run := range limited {
		if run != nil {
			limitedClaims++
		}
	}
	if limitedClaims != 1 {
		t.Fatalf("capacity-limited claims = %d, want 1", limitedClaims)
	}
}
