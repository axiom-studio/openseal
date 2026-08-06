//go:build integration

package runtime

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresObjectiveExecutionPolicySerializesReplicaClaims(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	schema := "openseal_objective_policy_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scope := Scope{Kind: "tenant", ID: "postgres-objective-policy"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "portfolio-team"}
	portfolio := NewPortfolioService(primary)
	limited, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Limited", Goal: "Run one occurrence at a time", Status: ObjectiveStatusActive,
		ExecutionPolicy: &ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"agent-one", "agent-two"} {
		if _, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, ObjectiveID: limited.ID, Owner: owner, AssignedAgentID: agentID,
			Goal: "Process limited work", Source: RunSourceObjective,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stores := []*PostgresStore{primary, replica}
	start := make(chan struct{})
	results := make(chan *AgentRun, len(stores))
	errs := make(chan error, len(stores))
	var wait sync.WaitGroup
	for index, store := range stores {
		wait.Add(1)
		go func(index int, store *PostgresStore) {
			defer wait.Done()
			<-start
			claimed, claimErr := store.ClaimNextAgentRun(ctx, AgentRunClaim{
				Scope: scope, WorkerID: "worker-" + string(rune('a'+index)), Now: time.Now().UTC(),
				LeaseDuration: time.Minute, AgingInterval: time.Minute,
			})
			results <- claimed
			errs <- claimErr
		}(index, store)
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for claimErr := range errs {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	claimedCount := 0
	for claimed := range results {
		if claimed != nil {
			claimedCount++
		}
	}
	if claimedCount != 1 {
		t.Fatalf("replica claims = %d, want exactly one", claimedCount)
	}
	decision, err := primary.ClaimNextAgentRunWithDecision(ctx, AgentRunClaim{
		Scope: scope, WorkerID: "observer", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || decision == nil || decision.Outcome != AgentRunAdmissionBackpressured {
		t.Fatalf("backpressure decision = %#v, %v", decision, err)
	}
	found := false
	for _, block := range decision.Blocks {
		if block.Reason == AgentRunAdmissionReasonObjectiveCapacity && block.ObjectiveID == limited.ID && block.Active == 1 && block.Limit == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("backpressure decision lacks Objective evidence: %#v", decision)
	}
}
