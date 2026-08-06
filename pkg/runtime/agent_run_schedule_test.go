package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateAgentRunInitializesDurableQueueMetadata(t *testing.T) {
	store := NewMemoryStore()
	service := NewPortfolioService(store)
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	available := now.Add(time.Hour)
	deadline := now.Add(2 * time.Hour)
	run, err := service.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: Scope{Kind: "local", ID: "test"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "scheduled work", Source: RunSourceSchedule,
		AvailableAt: &available, Deadline: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != AgentRunStatusQueued || !run.AvailableAt.Equal(available) || !run.QueueEnteredAt.Equal(now) ||
		run.Deadline == nil || !run.Deadline.Equal(deadline) || run.Revision != 1 {
		t.Fatalf("unexpected queued run: %#v", run)
	}
}

func TestAgentRunSchedulingUsesAgingDeadlineAndFIFO(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	oldLow := &AgentRun{ID: "old", Priority: 1, QueueEnteredAt: now.Add(-10 * time.Minute)}
	newHigh := &AgentRun{ID: "new", Priority: 5, QueueEnteredAt: now.Add(-time.Minute)}
	if !agentRunSchedulesBefore(oldLow, newHigh, now, time.Minute) {
		t.Fatal("wait aging did not prevent starvation")
	}
	early := now.Add(time.Hour)
	late := now.Add(2 * time.Hour)
	left := &AgentRun{ID: "left", Priority: 5, QueueEnteredAt: now, Deadline: &early}
	right := &AgentRun{ID: "right", Priority: 5, QueueEnteredAt: now, Deadline: &late}
	if !agentRunSchedulesBefore(left, right, now, time.Minute) {
		t.Fatal("earlier deadline did not break an equal-score tie")
	}
	left.Deadline, right.Deadline = nil, nil
	left.QueueEnteredAt = now.Add(-time.Second)
	if !agentRunSchedulesBefore(left, right, now, time.Minute) {
		t.Fatal("FIFO did not break an equal-score tie")
	}
}

func TestPortfolioClaimLimitsOwnerAndObjectiveAcrossAssignedAgents(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "portfolio-capacity.db"))
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
			scope := Scope{Kind: "tenant", ID: "portfolio-capacity-" + testCase.name}
			now := time.Date(2026, 8, 7, 4, 0, 0, 0, time.UTC)
			portfolio := NewPortfolioService(store)
			portfolio.now = func() time.Time { return now }
			team := ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"}
			otherTeam := ObjectiveOwner{Type: OwnerTypeTeam, ID: "support-team"}
			objectiveA, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: team, Title: "Market research", Goal: "Track the market", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			objectiveB, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: team, Title: "Lead follow-up", Goal: "Follow up with leads", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			objectiveC, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: otherTeam, Title: "Support", Goal: "Handle customer questions", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			create := func(objective *Objective, agent string, priority int) *AgentRun {
				t.Helper()
				run, createErr := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
					Scope: scope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: agent,
					Goal: "Work on " + objective.Title, Source: RunSourceObjective, Priority: priority,
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

			ownerClaim := AgentRunClaim{
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

			objectiveScope := Scope{Kind: "tenant", ID: "objective-capacity-" + testCase.name}
			objectivePortfolio := NewPortfolioService(store)
			objectivePortfolio.now = func() time.Time { return now }
			objectiveOwner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "delivery-team"}
			left, err := objectivePortfolio.CreateObjective(ctx, CreateObjectiveRequest{
				Scope: objectiveScope, Owner: objectiveOwner, Title: "Release", Goal: "Release safely", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			right, err := objectivePortfolio.CreateObjective(ctx, CreateObjectiveRequest{
				Scope: objectiveScope, Owner: objectiveOwner, Title: "Documentation", Goal: "Publish docs", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			createObjectiveRun := func(objective *Objective, agent string, priority int) *AgentRun {
				t.Helper()
				run, createErr := objectivePortfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
					Scope: objectiveScope, ObjectiveID: objective.ID, Owner: objective.Owner, AssignedAgentID: agent,
					Goal: "Work on " + objective.Title, Source: RunSourceObjective, Priority: priority,
				})
				if createErr != nil {
					t.Fatal(createErr)
				}
				return run
			}
			leftFirst := createObjectiveRun(left, "developer-one", 10)
			createObjectiveRun(left, "developer-two", 9)
			rightFirst := createObjectiveRun(right, "writer", 1)
			objectiveClaim := AgentRunClaim{
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
