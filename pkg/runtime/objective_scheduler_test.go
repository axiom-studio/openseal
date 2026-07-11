package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestObjectiveCadenceUsesExplicitTimezoneAndValidates(t *testing.T) {
	from := time.Date(2026, time.March, 8, 6, 30, 0, 0, time.UTC)
	next, err := (&ObjectiveCadence{
		Type: ObjectiveCadenceDaily, TimeOfDay: "09:00", Timezone: "America/New_York",
	}).Next(from)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.March, 8, 13, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	if err := (&ObjectiveCadence{Type: ObjectiveCadenceWeekly, DayOfWeek: "noday"}).Validate(); err == nil {
		t.Fatal("expected invalid weekday to fail")
	}
}

func TestObjectiveSchedulerCreatesCanonicalBoundedRunAndBackpressures(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(10)
	now := time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	due := now.Add(-time.Minute)
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "scheduler"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"},
		Title: "Operate", Goal: "Inspect production health", Status: ObjectiveStatusActive,
		Budget: &BudgetPolicy{MaxTurns: 10}, Cadence: &ObjectiveCadence{
			Type: ObjectiveCadenceInterval, IntervalSeconds: 300, AssignedAgentID: "sre",
			RunBudget: &BudgetPolicy{MaxTurns: 2},
		},
		NextEvaluationAt: &due, IdempotencyKey: "operate",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewObjectiveScheduler(store)
	scheduler.now = func() time.Time { return now }
	result, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scheduled != 1 || result.Examined != 1 {
		t.Fatalf("result = %#v", result)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %#v, err = %v", runs, err)
	}
	if runs[0].Source != RunSourceSchedule || runs[0].Budget == nil || runs[0].Budget.MaxTurns != 2 || runs[0].AssignedAgentID != "sre" {
		t.Fatalf("run = %#v", runs[0])
	}
	loaded, err := portfolio.GetObjective(ctx, objective.Scope, objective.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := due.Add(5 * time.Minute)
	if loaded.NextEvaluationAt == nil || !loaded.NextEvaluationAt.Equal(wantNext) || loaded.BudgetAllocations[runs[0].ID].MaxTurns != 2 {
		t.Fatalf("objective = %#v", loaded)
	}
	scheduler.now = func() time.Time { return wantNext.Add(time.Minute) }
	blocked, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || blocked.Backpressured != 1 || len(runs) != 1 {
		t.Fatalf("blocked = %#v, err = %v", blocked, err)
	}
}

func TestObjectiveSchedulerRecoversDueWorkAfterSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 5, 0, 0, 0, time.UTC)
	due := now.Add(-time.Second)
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "restart"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "ops"},
		Title: "Review", Goal: "Review incidents", Status: ObjectiveStatusActive,
		Cadence:          &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 60, AssignedAgentID: "lead"},
		NextEvaluationAt: &due,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	scheduler := NewObjectiveScheduler(reopened)
	scheduler.now = func() time.Time { return now }
	result, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || result.Scheduled != 1 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	runs, err := reopened.ListAgentRuns(ctx, AgentRunFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %#v, err = %v", runs, err)
	}
}
