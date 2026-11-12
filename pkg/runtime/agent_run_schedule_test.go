package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestCreateAgentRunInitializesDurableQueueMetadata(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	service := runtimeportfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	available := now.Add(time.Hour)
	deadline := now.Add(2 * time.Hour)
	run, err := service.CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: models.Scope{Kind: "local", ID: "test"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "scheduled work", Source: models.RunSourceSchedule,
		AvailableAt: &available, Deadline: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.AgentRunStatusQueued || !run.AvailableAt.Equal(available) || !run.QueueEnteredAt.Equal(now) ||
		run.Deadline == nil || !run.Deadline.Equal(deadline) || run.Revision != 1 {
		t.Fatalf("unexpected queued run: %#v", run)
	}
}

func TestPortfolioClaimLimitsOwnerAndObjectiveAcrossAssignedAgents(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "portfolio-capacity.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, closeStore := testCase.open(t)
			defer closeStore()
			ctx := t.Context()
			scope := models.Scope{Kind: "tenant", ID: "portfolio-capacity-" + testCase.name}
			now := time.Date(2026, 8, 7, 4, 0, 0, 0, time.UTC)
			portfolio := runtimeportfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
			team := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research-team"}
			otherTeam := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "support-team"}
			objectiveA, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: team, Title: "Market research", Goal: "Track the market", Status: models.ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			objectiveB, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: team, Title: "Lead follow-up", Goal: "Follow up with leads", Status: models.ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			objectiveC, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: otherTeam, Title: "Support", Goal: "Handle customer questions", Status: models.ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			create := func(objective *models.Objective, agent string, priority int) *models.AgentRun {
				t.Helper()
				run, createErr := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
					Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: agent,
					Goal: "Work on " + objective.Title, Source: models.RunSourceObjective, Priority: priority,
				})
				if createErr != nil {
					t.Fatal(createErr)
				}
				return run
			}
			firstA := create(objectiveA, "researcher-one", 10)
			create(objectiveA, "researcher-two", 9)
			create(objectiveB, "researcher-three", 8)
			firstC := create(objectiveC, "supporter", 1)

			ownerClaim := models.AgentRunClaim{
				Scope: scope, WorkerID: "owner-worker-one", Now: now, LeaseDuration: time.Minute,
				AgingInterval: time.Minute, MaxActiveForOwner: 1,
			}
			claimed, err := store.ClaimNextAgentRun(ctx, ownerClaim)
			if err != nil || claimed == nil || claimed.ID != firstA.ID {
				t.Fatalf("first owner claim = %#v, %v", claimed, err)
			}
			ownerClaim.WorkerID = "owner-worker-two"
			claimed, err = store.ClaimNextAgentRun(ctx, ownerClaim)
			if err != nil || claimed == nil || claimed.ID != firstC.ID {
				t.Fatalf("independent owner claim = %#v, %v", claimed, err)
			}

			objectiveScope := models.Scope{Kind: "tenant", ID: "objective-capacity-" + testCase.name}
			objectivePortfolio := runtimeportfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
			objectiveOwner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "delivery-team"}
			left, err := objectivePortfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: objectiveScope, Owner: objectiveOwner, Title: "Release", Goal: "Release safely", Status: models.ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			right, err := objectivePortfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: objectiveScope, Owner: objectiveOwner, Title: "Documentation", Goal: "Publish docs", Status: models.ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			createObjectiveRun := func(objective *models.Objective, agent string, priority int) *models.AgentRun {
				t.Helper()
				run, createErr := objectivePortfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
					Scope: objectiveScope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: agent,
					Goal: "Work on " + objective.Title, Source: models.RunSourceObjective, Priority: priority,
				})
				if createErr != nil {
					t.Fatal(createErr)
				}
				return run
			}
			leftFirst := createObjectiveRun(left, "developer-one", 10)
			createObjectiveRun(left, "developer-two", 9)
			rightFirst := createObjectiveRun(right, "writer", 1)
			objectiveClaim := models.AgentRunClaim{
				Scope: objectiveScope, WorkerID: "objective-worker-one", Now: now, LeaseDuration: time.Minute,
				AgingInterval: time.Minute, MaxActiveForObjective: 1,
			}
			claimed, err = store.ClaimNextAgentRun(ctx, objectiveClaim)
			if err != nil || claimed == nil || claimed.ID != leftFirst.ID {
				t.Fatalf("first objective claim = %#v, %v", claimed, err)
			}
			objectiveClaim.WorkerID = "objective-worker-two"
			claimed, err = store.ClaimNextAgentRun(ctx, objectiveClaim)
			if err != nil || claimed == nil || claimed.ID != rightFirst.ID {
				t.Fatalf("independent objective claim = %#v, %v", claimed, err)
			}
		})
	}
}

func TestObjectiveExecutionPolicyImmediatelyGovernsQueuedWork(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "objective-policy.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, closeStore := testCase.open(t)
			defer closeStore()
			ctx := t.Context()
			scope := models.Scope{Kind: "tenant", ID: "objective-policy-" + testCase.name}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "portfolio-team"}
			now := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
			portfolio := runtimeportfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
			limited, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Primary", Goal: "Ship primary work", Status: models.ObjectiveStatusActive,
				ExecutionPolicy: &models.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			independent, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Independent", Goal: "Keep independent work moving", Status: models.ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			create := func(objective *models.Objective, agent string, priority int) *models.AgentRun {
				t.Helper()
				run, createErr := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
					Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: agent,
					Goal: "Work on " + objective.Title, Source: models.RunSourceObjective, Priority: priority,
				})
				if createErr != nil {
					t.Fatal(createErr)
				}
				return run
			}
			first := create(limited, "agent-one", 10)
			second := create(limited, "agent-two", 9)
			other := create(independent, "agent-three", 1)
			claim := models.AgentRunClaim{Scope: scope, WorkerID: "worker-one", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute}
			claimed, err := store.ClaimNextAgentRun(ctx, claim)
			if err != nil || claimed == nil || claimed.ID != first.ID {
				t.Fatalf("first claim = %#v, %v", claimed, err)
			}
			claim.WorkerID = "worker-two"
			claimed, err = store.ClaimNextAgentRun(ctx, claim)
			if err != nil || claimed == nil || claimed.ID != other.ID {
				t.Fatalf("policy did not backpressure the second Objective Run: %#v, %v", claimed, err)
			}

			limited, err = portfolio.UpdateObjective(ctx, scope, limited.ID, runtimeportfolio.UpdateObjectiveRequest{
				ExpectedRevision: limited.Revision,
				ExecutionPolicy:  &models.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 2},
				Summary:          "Increase Objective concurrency",
			})
			if err != nil || limited.ExecutionPolicy.MaximumConcurrentRuns != 2 {
				t.Fatalf("update policy = %#v, %v", limited, err)
			}
			claim.WorkerID = "worker-three"
			claimed, err = store.ClaimNextAgentRun(ctx, claim)
			if err != nil || claimed == nil || claimed.ID != second.ID {
				t.Fatalf("amended policy did not admit existing queued work: %#v, %v", claimed, err)
			}
		})
	}
}

func TestAdmissionDecisionExplainsObjectiveBackpressureAndNextWake(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "admission-decision.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, closeStore := testCase.open(t)
			defer closeStore()
			ctx := t.Context()
			now := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
			scope := models.Scope{Kind: "tenant", ID: "admission-decision-" + testCase.name}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "autonomous-agent"}
			portfolio := runtimeportfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
			objective, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Bounded", Goal: "Run serially", Status: models.ObjectiveStatusActive,
				ExecutionPolicy: &models.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			firstRunID := ""
			for _, suffix := range []string{"one", "two"} {
				run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
					Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: "agent-" + suffix,
					Goal: "Bounded work " + suffix, Source: models.RunSourceObjective,
				})
				if err != nil {
					t.Fatal(err)
				}
				if firstRunID == "" {
					firstRunID = run.ID
				}
			}
			scheduler := agentrun.NewAgentRunScheduler(store, models.WithClock(func() time.Time { return now }))
			inspection, err := scheduler.Inspect(ctx, agentrun.AgentRunClaimRequest{Scope: scope})
			if err != nil || inspection == nil || inspection.Outcome != models.AgentRunAdmissionReady || inspection.Run == nil {
				t.Fatalf("ready inspection = %#v, %v", inspection, err)
			}
			stillQueued, err := portfolio.GetAgentRun(ctx, scope, firstRunID)
			if err != nil || stillQueued.Status != models.AgentRunStatusQueued || stillQueued.LeaseOwner != "" {
				t.Fatalf("inspection acquired authority: %#v, %v", stillQueued, err)
			}
			if run, err := scheduler.ClaimNext(ctx, agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker-one", LeaseDuration: time.Hour}); err != nil || run == nil {
				t.Fatalf("initial claim = %#v, %v", run, err)
			}
			decision, err := scheduler.ClaimNextDecision(ctx, agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker-two", LeaseDuration: time.Hour})
			if err != nil || decision == nil || decision.Outcome != models.AgentRunAdmissionBackpressured || decision.Run != nil {
				t.Fatalf("backpressure decision = %#v, %v", decision, err)
			}
			found := false
			for _, block := range decision.Blocks {
				if block.Reason == models.AgentRunAdmissionReasonObjectiveCapacity && block.ObjectiveID == objective.ID && block.Active == 1 && block.Limit == 1 {
					found = true
				}
			}
			if !found || decision.NextWakeAt == nil || !decision.NextWakeAt.Equal(now.Add(time.Hour)) {
				t.Fatalf("decision lacks capacity evidence or lease wake: %#v", decision)
			}
		})
	}
}

func TestObjectiveResourceCapacityAtomicallyGovernsQueuedWork(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "objective-resources.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, closeStore := testCase.open(t)
			defer closeStore()
			ctx := t.Context()
			now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
			scope := models.Scope{Kind: "tenant", ID: "objective-resources-" + testCase.name}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "research-agent"}
			portfolio := runtimeportfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
			objective, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Browser research", Goal: "Research without sharing sessions", Status: models.ObjectiveStatusActive,
				ExecutionPolicy: &models.ObjectiveExecutionPolicy{ResourceCapacities: map[string]int{"browser": 1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var runIDs []string
			for _, agentID := range []string{"researcher-one", "researcher-two"} {
				run, createErr := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
					Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: agentID,
					Goal: "Browse independently", Source: models.RunSourceObjective, ResourceRequirements: map[string]int{"browser": 1},
				})
				if createErr != nil {
					t.Fatal(createErr)
				}
				runIDs = append(runIDs, run.ID)
			}
			scheduler := agentrun.NewAgentRunScheduler(store, models.WithClock(func() time.Time { return now }))
			first, err := scheduler.ClaimNext(ctx, agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker-one", LeaseDuration: time.Hour})
			if err != nil || first == nil || first.ResourceRequirements["browser"] != 1 {
				t.Fatalf("first resource claim = %#v, %v", first, err)
			}
			queuedRunID := runIDs[0]
			if first.ID == queuedRunID {
				queuedRunID = runIDs[1]
			}
			decision, err := scheduler.ClaimNextDecision(ctx, agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker-two", LeaseDuration: time.Hour})
			if err != nil || decision == nil || decision.Outcome != models.AgentRunAdmissionBackpressured || decision.Run != nil {
				t.Fatalf("resource admission decision = %#v, %v", decision, err)
			}
			found := false
			for _, block := range decision.Blocks {
				if block.RunID == queuedRunID && block.Reason == models.AgentRunAdmissionReasonResourceCapacity && block.Resource == "browser" &&
					block.Requested == 1 && block.Reserved == 1 && block.Capacity == 1 {
					found = true
				}
			}
			if !found || decision.NextWakeAt == nil || !decision.NextWakeAt.Equal(now.Add(time.Hour)) {
				t.Fatalf("resource decision lacks evidence or wake: %#v", decision)
			}
			objective, err = portfolio.UpdateObjective(ctx, scope, objective.ID, runtimeportfolio.UpdateObjectiveRequest{
				ExpectedRevision: objective.Revision,
				ExecutionPolicy:  &models.ObjectiveExecutionPolicy{ResourceCapacities: map[string]int{"browser": 2}},
				Summary:          "Increase browser capacity",
			})
			if err != nil {
				t.Fatal(err)
			}
			second, err := scheduler.ClaimNext(ctx, agentrun.AgentRunClaimRequest{Scope: scope, WorkerID: "worker-three", LeaseDuration: time.Hour})
			if err != nil || second == nil || second.ID != queuedRunID {
				t.Fatalf("amended resource policy did not admit queued work: %#v, %v", second, err)
			}
		})
	}
}
