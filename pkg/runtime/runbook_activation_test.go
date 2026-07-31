package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestRunbookActivationIdempotentCreatesConvergeAcrossRace(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Hourly research", Goal: "Research once per hour", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "hourly", Status: RunbookActivationActive,
		Trigger:        runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC", MaximumOccurrences: 5}},
		IdempotencyKey: "replacement-for-retired-generation",
	}
	const callers = 16
	start := make(chan struct{})
	results := make(chan *RunbookActivation, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			activation, createErr := NewRunbookActivationService(store).Create(ctx, request)
			results <- activation
			errs <- createErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for createErr := range errs {
		if createErr != nil {
			t.Fatalf("concurrent idempotent create failed: %v", createErr)
		}
	}
	var id string
	for activation := range results {
		if activation == nil {
			t.Fatal("concurrent idempotent create returned nil activation")
		}
		if id == "" {
			id = activation.ID
		} else if activation.ID != id {
			t.Fatalf("concurrent idempotent creates produced %q and %q", id, activation.ID)
		}
	}
	activations, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(activations) != 1 {
		t.Fatalf("concurrent idempotent creates persisted %#v err=%v", activations, err)
	}
}

func TestResolveRunbookDetailUsesExactPinnedHistoricDefinition(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	registry := kernelagent.NewRegistryWithStore(kernelagent.NewMemoryStore())
	for _, version := range []string{"1", "2"} {
		_, err := registry.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
			ID: "rowan", Version: version, DisplayName: "Rowan", Purpose: "Research communities", SystemPrompt: "Research carefully.",
			Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
			Runbook: &runbook.Definition{
				APIVersion: runbook.APIVersion, ID: "community-review", Version: version, Name: "Community review",
				Entrypoints: map[string]string{"review": "done"},
				Steps:       map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: owner.ID, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "rowan", ActiveVersion: "2",
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "test"); err != nil {
		t.Fatal(err)
	}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful advice", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "daily-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := ResolveRunbookDetail(ctx, store, registry, scope, activation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Activation.ID != activation.ID || detail.Definition.Version != "1" {
		t.Fatalf("detail = %#v", detail)
	}
}

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

func TestActiveRunbookActivationMaterializesReportingChannel(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful advice", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewRunbookActivationService(store)
	request := CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "daily", IdempotencyKey: "rowan:daily",
		Trigger: runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "review",
			Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"},
			Reporting: &runbook.ReportingPolicy{
				Channel: "community-work", Title: "Community work",
				Milestones: []runbook.ReportingMilestone{runbook.ReportingStarted, runbook.ReportingApprovalRequired, runbook.ReportingCompleted, runbook.ReportingFailed},
			},
		},
	}
	activation, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	channels, err := store.ListConversations(ctx, ConversationFilter{Scope: scope, Owner: &owner})
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].Title != "Community work" {
		t.Fatalf("channels after activation = %#v", channels)
	}

	replayed, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	channels, err = store.ListConversations(ctx, ConversationFilter{Scope: scope, Owner: &owner})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != activation.ID || len(channels) != 1 {
		t.Fatalf("replayed=%#v channels=%#v", replayed, channels)
	}
}

func TestPausedRunbookActivationDefersReportingChannelUntilResumed(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Publish research", Goal: "Publish a cited brief", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewRunbookActivationService(store)
	activation, err := service.Create(ctx, CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "researcher",
		DefinitionID: "weekly-brief", DefinitionVersion: "1", TriggerID: "weekly", Status: RunbookActivationPaused,
		Trigger: runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "publish",
			Schedule: &runbook.Schedule{Cron: "0 0 9 * * 1", Timezone: "UTC"},
			Reporting: &runbook.ReportingPolicy{
				Channel: "research-work", Title: "Research work",
				Milestones: []runbook.ReportingMilestone{runbook.ReportingStarted, runbook.ReportingCompleted},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	channels, err := store.ListConversations(ctx, ConversationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(channels) != 0 {
		t.Fatalf("paused channels=%#v err=%v", channels, err)
	}
	activation, err = service.Update(ctx, scope, activation.ID, UpdateRunbookActivationRequest{
		ExpectedRevision: activation.Revision, Status: RunbookActivationActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	channels, err = store.ListConversations(ctx, ConversationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(channels) != 1 || channels[0].Title != "Research work" {
		t.Fatalf("resumed channels=%#v err=%v", channels, err)
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

func TestRunbookActivationLifecycleIsRevisionBoundAndRetirementIsFinal(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Outcome", Goal: "Do useful work", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewRunbookActivationService(store)
	activation, err := service.Create(ctx, CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan", DefinitionID: "work",
		DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := StartRunbookActivation(ctx, store, scope, activation.ID, StartRunbookActivationRequest{IdempotencyKey: "manual-run"})
	if err != nil || started.Run == nil {
		t.Fatalf("started=%#v err=%v", started, err)
	}
	if pin, ok := started.Run.Plan["runbook"].(map[string]interface{}); !ok || pin["id"] != "work" || pin["version"] != "1" || pin["trigger"] != "daily" {
		t.Fatalf("manual Run pin=%#v", started.Run.Plan)
	}
	paused, err := service.Update(ctx, scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: RunbookActivationPaused})
	if err != nil || paused.Status != RunbookActivationPaused || paused.Revision != 2 {
		t.Fatalf("paused=%#v err=%v", paused, err)
	}
	if _, err = StartRunbookActivation(ctx, store, scope, activation.ID, StartRunbookActivationRequest{IdempotencyKey: "paused-run"}); !errors.Is(err, ErrRunbookActivationInactive) {
		t.Fatalf("paused start error=%v", err)
	}
	if _, err = service.Update(ctx, scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: 1, Status: RunbookActivationActive}); !errors.Is(err, ErrRunbookActivationRevision) {
		t.Fatalf("stale update error=%v", err)
	}
	retired, err := service.Update(ctx, scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: paused.Revision, Status: RunbookActivationRetired})
	if err != nil || retired.Status != RunbookActivationRetired {
		t.Fatalf("retired=%#v err=%v", retired, err)
	}
	if _, err = service.Update(ctx, scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: retired.Revision, Status: RunbookActivationActive}); err == nil {
		t.Fatal("retired Runbook resumed")
	}
}
