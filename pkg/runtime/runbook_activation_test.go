package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestRunbookActivationLivesUnderObjectiveAndOwnsSchedule(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	portfolio := NewPortfolioService(store)
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful woodworking advice", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewRunbookActivationService(store)
	activation, err := service.Create(ctx, CreateRunbookActivationRequest{
		ID: "daily-community-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan",
		DefinitionID: "community-review", DefinitionVersion: "1.0.0", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{
			Cron: "0 0 0 * * *", Timezone: "UTC", JitterSeconds: 86399,
		}},
		Input: map[string]interface{}{"communities": []interface{}{"r/woodworking"}}, MaximumConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if activation.ObjectiveID != objective.ID || activation.Trigger.Schedule.JitterSeconds != 86399 {
		t.Fatalf("objective=%#v activation=%#v", objective, activation)
	}
	listed, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: scope, ObjectiveID: objective.ID})
	if err != nil || len(listed) != 1 || listed[0].ID != activation.ID {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	page, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 1, Offset: 1})
	if err != nil || len(page) != 0 {
		t.Fatalf("offset page=%#v err=%v", page, err)
	}
}

func TestSQLiteRunbookActivationSurvivesRestartWithScheduleCursor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful advice", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan", DefinitionID: "community-review",
		DefinitionVersion: "1", TriggerID: "daily", IdempotencyKey: "rowan:daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC", JitterSeconds: 86399}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := activation.Trigger.Schedule.NextBase(activation.CreatedAt)
	due, _ := activation.Trigger.Schedule.DueAt(activation.ID+":"+activation.TriggerID, base)
	updated := cloneRunbookActivation(activation)
	updated.NextOccurrenceBase, updated.NextRunAt = &base, &due
	updated.Revision++
	updated.UpdatedAt = updated.UpdatedAt.Add(1)
	if err := store.UpdateRunbookActivation(ctx, updated, activation.Revision); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetRunbookActivation(ctx, scope, activation.ID)
	if err != nil || restored == nil || restored.ObjectiveID != objective.ID || restored.NextRunAt == nil || !restored.NextRunAt.Equal(due) {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
	replayed, err := NewRunbookActivationService(restarted).Create(ctx, CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan", DefinitionID: "community-review",
		DefinitionVersion: "1", TriggerID: "daily", IdempotencyKey: "rowan:daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC", JitterSeconds: 86399}},
	})
	if err != nil || replayed.ID != activation.ID {
		t.Fatalf("idempotent replay=%#v err=%v", replayed, err)
	}
}

func TestRunbookActivationCannotCrossObjectiveOwner(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}, Title: "Outcome", Goal: "Do useful work", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}, ObjectiveID: objective.ID, AssignedAgentID: "other",
		DefinitionID: "work", DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err == nil {
		t.Fatal("cross-owner Runbook activation succeeded")
	}
}
