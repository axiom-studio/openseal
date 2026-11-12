//go:build integration

package portfolio_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
)

func TestPostgresObjectiveExecutionPolicySerializesReplicaClaims(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	primary, pgSchema := kerneltest.OpenPostgres(t, "openseal_objective_policy_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scope := models.Scope{Kind: "tenant", ID: "postgres-objective-policy"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "portfolio-team"}
	portfolio := runtimeportfolio.NewPortfolioService(primary)
	limited, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Limited", Goal: "Run one occurrence at a time", Status: models.ObjectiveStatusActive,
		ExecutionPolicy: &models.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"agent-one", "agent-two"} {
		if _, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: scope, ObjectiveID: limited.ID, Owner: owner, AssignedAgentID: agentID,
			Goal: "Process limited work", Source: models.RunSourceObjective,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stores := []*postgres.Store{primary, replica}
	start := make(chan struct{})
	results := make(chan *models.AgentRun, len(stores))
	errs := make(chan error, len(stores))
	var wait sync.WaitGroup
	for index, store := range stores {
		wait.Add(1)
		go func(index int, store *postgres.Store) {
			defer wait.Done()
			<-start
			claimed, claimErr := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
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
	decision, err := primary.ClaimNextAgentRunWithDecision(ctx, models.AgentRunClaim{
		Scope: scope, WorkerID: "observer", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || decision == nil || decision.Outcome != models.AgentRunAdmissionBackpressured {
		t.Fatalf("backpressure decision = %#v, %v", decision, err)
	}
	found := false
	for _, block := range decision.Blocks {
		if block.Reason == models.AgentRunAdmissionReasonObjectiveCapacity && block.ObjectiveID == limited.ID && block.Active == 1 && block.Limit == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("backpressure decision lacks Objective evidence: %#v", decision)
	}

	resourceScope := models.Scope{Kind: "tenant", ID: "postgres-objective-resources"}
	resourceObjective, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: resourceScope, Owner: owner, Title: "Reserved browser", Goal: "Use one browser at a time", Status: models.ObjectiveStatusActive,
		ExecutionPolicy: &models.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 2, ResourceCapacities: map[string]int{"browser": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"browser-agent-one", "browser-agent-two"} {
		if _, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: resourceScope, ObjectiveID: resourceObjective.ID, Owner: owner, AssignedAgentID: agentID,
			Goal: "Browse independently", Source: models.RunSourceObjective, ResourceRequirements: map[string]int{"browser": 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	resourceStart := make(chan struct{})
	resourceResults := make(chan *models.AgentRun, len(stores))
	resourceErrors := make(chan error, len(stores))
	for index, store := range stores {
		wait.Add(1)
		go func(index int, store *postgres.Store) {
			defer wait.Done()
			<-resourceStart
			claimed, claimErr := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
				Scope: resourceScope, WorkerID: "resource-worker-" + string(rune('a'+index)), Now: time.Now().UTC(),
				LeaseDuration: time.Minute, AgingInterval: time.Minute,
			})
			resourceResults <- claimed
			resourceErrors <- claimErr
		}(index, store)
	}
	close(resourceStart)
	wait.Wait()
	close(resourceResults)
	close(resourceErrors)
	for claimErr := range resourceErrors {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	resourceClaimedCount := 0
	for claimed := range resourceResults {
		if claimed != nil {
			resourceClaimedCount++
		}
	}
	if resourceClaimedCount != 1 {
		t.Fatalf("resource replica claims = %d, want exactly one", resourceClaimedCount)
	}
	resourceDecision, err := primary.ClaimNextAgentRunWithDecision(ctx, models.AgentRunClaim{
		Scope: resourceScope, WorkerID: "resource-observer", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || resourceDecision == nil || resourceDecision.Outcome != models.AgentRunAdmissionBackpressured {
		t.Fatalf("resource backpressure decision = %#v, %v", resourceDecision, err)
	}
	found = false
	for _, block := range resourceDecision.Blocks {
		if block.Reason == models.AgentRunAdmissionReasonResourceCapacity && block.ObjectiveID == resourceObjective.ID &&
			block.Resource == "browser" && block.Requested == 1 && block.Reserved == 1 && block.Capacity == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("backpressure decision lacks resource evidence: %#v", resourceDecision)
	}
}
