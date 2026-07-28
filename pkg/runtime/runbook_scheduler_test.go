package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestRunbookSchedulerCreatesExactlyOneAttributedRunPerCronOccurrence(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Useful participation", Goal: "Contribute useful advice", Status: ObjectiveStatusActive, Priority: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	activationService := NewRunbookActivationService(store)
	activationService.now = func() time.Time { return now }
	activation, err := activationService.Create(ctx, CreateRunbookActivationRequest{
		ID: "daily-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan",
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "daily", MaximumConcurrent: 2,
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{
			Cron: "0 0 0 * * *", Timezone: "UTC", JitterSeconds: 86399,
		}}, Input: map[string]interface{}{"communities": []interface{}{"r/woodworking"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewRunbookScheduler(store)
	scheduler.now = func() time.Time { return now }
	initialized, err := scheduler.ReconcileScope(ctx, scope, 50)
	if err != nil || initialized.Initialized != 1 || initialized.Scheduled != 0 {
		t.Fatalf("initialize=%#v err=%v", initialized, err)
	}
	activation, _ = store.GetRunbookActivation(ctx, scope, activation.ID)
	if activation.NextOccurrenceBase == nil || activation.NextRunAt == nil {
		t.Fatalf("activation cursor=%#v", activation)
	}
	now = activation.NextRunAt.Add(time.Second)
	result, err := scheduler.ReconcileScope(ctx, scope, 50)
	if err != nil || result.Scheduled != 1 {
		t.Fatalf("schedule=%#v err=%v", result, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	created := runs[0]
	if created.Entrypoint != "review" || created.AssignedAgentID != "rowan" || created.Context["runbookActivationId"] != activation.ID || created.Context["runbookTriggerId"] != "daily" {
		t.Fatalf("scheduled Run=%#v", created)
	}
	if communities, ok := created.Context["communities"].([]interface{}); !ok || len(communities) != 1 {
		t.Fatalf("Runbook input=%#v", created.Context)
	}
	replayed, err := scheduler.ReconcileScope(ctx, scope, 50)
	if err != nil || replayed.Scheduled != 0 {
		t.Fatalf("reconcile replay=%#v err=%v", replayed, err)
	}
	runs, _ = store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 10})
	if len(runs) != 1 {
		t.Fatalf("duplicate Runs=%#v", runs)
	}
}

func TestRunbookSchedulerKeepsSiblingRunbooksIndependent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, _ := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Outcome", Goal: "Pursue several methods", Status: ObjectiveStatusActive,
	})
	service := NewRunbookActivationService(store)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	for _, id := range []string{"scan", "report"} {
		_, err := service.Create(ctx, CreateRunbookActivationRequest{
			ID: id, Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan", DefinitionID: id,
			DefinitionVersion: "1", TriggerID: "daily", MaximumConcurrent: 1,
			Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	scheduler := NewRunbookScheduler(store)
	scheduler.now = func() time.Time { return now }
	if result, err := scheduler.ReconcileScope(ctx, scope, 50); err != nil || result.Initialized != 2 {
		t.Fatalf("initialize=%#v err=%v", result, err)
	}
	values, _ := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: scope, ObjectiveID: objective.ID})
	now = values[0].NextRunAt.Add(time.Second)
	if result, err := scheduler.ReconcileScope(ctx, scope, 50); err != nil || result.Scheduled != 2 {
		t.Fatalf("schedule siblings=%#v err=%v", result, err)
	}
}
