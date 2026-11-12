package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestRunbookActivationIdempotentCreatesConvergeAcrossRace(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Hourly research", Goal: "Research once per hour", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "hourly", Status: models.RunbookActivationActive,
		Trigger:        runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC", MaximumOccurrences: 5}},
		IdempotencyKey: "replacement-for-retired-generation",
	}
	const callers = 16
	start := make(chan struct{})
	results := make(chan *models.RunbookActivation, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			activation, createErr := routine.NewRunbookActivationService(store).Create(ctx, request)
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
	activations, err := store.ListRunbookActivations(ctx, models.RunbookActivationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(activations) != 1 {
		t.Fatalf("concurrent idempotent creates persisted %#v err=%v", activations, err)
	}
}

func TestResolveRunbookDetailUsesExactPinnedHistoricDefinition(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
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
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful advice", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		ID: "daily-review", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "community-review", DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := routine.ResolveRunbookDetail(ctx, store, registry, scope, activation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Activation.ID != activation.ID || detail.Definition.Version != "1" {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestResolvedVerificationBlocksManualAndScheduledActivation(t *testing.T) {
	ctx := t.Context()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "verification.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := models.Scope{Kind: "tenant", ID: "verification"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "operator"}
	definition := sqliteAgentDefinition("1.0.0")
	definition.SkillRequirements = []kernelagent.SkillRequirement{{SkillID: "missing", VersionConstraint: "1.0.0", RequiredActions: []string{"execute"}}}
	definition.Authority.AllowedSkillIDs = []string{"missing"}
	definition.Runbook = &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "verified-operation", Version: "1.0.0", Name: "Verified operation",
		Entrypoints: map[string]string{"operate": "execute"},
		Triggers: map[string]runbook.Trigger{"hourly": {
			Kind: runbook.TriggerSchedule, Entrypoint: "operate", ObjectiveID: "operate",
			Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"},
		}},
		Steps: map[string]runbook.Step{
			"execute": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "missing", SkillVersion: "1.0.0", Action: "execute", ResultPath: "/result", Next: "done"}},
			"done":    {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	registry := kernelagent.NewRegistryWithStore(store)
	if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: owner.ID, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "verification test"); err != nil {
		t.Fatal(err)
	}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Operate", Goal: "Operate safely", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		ID: "hourly", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: definition.Runbook.ID, DefinitionVersion: definition.Runbook.Version, TriggerID: "hourly", Trigger: definition.Runbook.Triggers["hourly"],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routine.StartRunbookActivation(ctx, store, scope, activation.ID, routine.StartRunbookActivationRequest{}); !errors.Is(err, routine.ErrRunbookActivationUnverified) {
		t.Fatalf("manual activation error = %v", err)
	}
	result, err := routine.NewRunbookScheduler(store).ReconcileScope(ctx, scope, 10)
	if err != nil || result.Suspended != 1 || result.Examined != 1 {
		t.Fatalf("schedule verification = %#v, %v", result, err)
	}
	paused, err := store.GetRunbookActivation(ctx, scope, activation.ID)
	if err != nil || paused.Status != models.RunbookActivationPaused {
		t.Fatalf("paused activation = %#v, %v", paused, err)
	}
}

func TestRunbookActivationLivesUnderObjectiveAndOwnsSchedule(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	portfolio := runtimeportfolio.NewPortfolioService(store)
	objective, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful woodworking advice", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := routine.NewRunbookActivationService(store)
	activation, err := service.Create(ctx, routine.CreateRunbookActivationRequest{
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
	listed, err := store.ListRunbookActivations(ctx, models.RunbookActivationFilter{Scope: scope, ObjectiveID: objective.ID})
	if err != nil || len(listed) != 1 || listed[0].ID != activation.ID {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	page, err := store.ListRunbookActivations(ctx, models.RunbookActivationFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 1, Offset: 1})
	if err != nil || len(page) != 0 {
		t.Fatalf("offset page=%#v err=%v", page, err)
	}
}

func TestActiveRunbookActivationMaterializesReportingChannel(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful advice", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := routine.NewRunbookActivationService(store)
	request := routine.CreateRunbookActivationRequest{
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
	seedTestSpace(t, store, scope)
	activation, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	channels, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, Owner: &owner})
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
	channels, err = store.ListConversations(ctx, models.ConversationFilter{Scope: scope, Owner: &owner})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != activation.ID || len(channels) != 1 {
		t.Fatalf("replayed=%#v channels=%#v", replayed, channels)
	}
}

func TestPausedRunbookActivationDefersReportingChannelUntilResumed(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	seedTestSpace(t, store, scope)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Publish research", Goal: "Publish a cited brief", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := routine.NewRunbookActivationService(store)
	activation, err := service.Create(ctx, routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "researcher",
		DefinitionID: "weekly-brief", DefinitionVersion: "1", TriggerID: "weekly", Status: models.RunbookActivationPaused,
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
	channels, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(channels) != 0 {
		t.Fatalf("paused channels=%#v err=%v", channels, err)
	}
	activation, err = service.Update(ctx, scope, activation.ID, routine.UpdateRunbookActivationRequest{
		ExpectedRevision: activation.Revision, Status: models.RunbookActivationActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	channels, err = store.ListConversations(ctx, models.ConversationFilter{Scope: scope, Owner: &owner})
	if err != nil || len(channels) != 1 || channels[0].Title != "Research work" {
		t.Fatalf("resumed channels=%#v err=%v", channels, err)
	}
}

func TestSQLiteRunbookActivationSurvivesRestartWithScheduleCursor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Build useful participation", Goal: "Contribute useful advice", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan", DefinitionID: "community-review",
		DefinitionVersion: "1", TriggerID: "daily", IdempotencyKey: "rowan:daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "review", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC", JitterSeconds: 86399}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := activation.Trigger.Schedule.NextBase(activation.CreatedAt)
	due, _ := activation.Trigger.Schedule.DueAt(activation.ID+":"+activation.TriggerID, base)
	updated := models.CloneRunbookActivation(activation)
	updated.NextOccurrenceBase, updated.NextRunAt = &base, &due
	updated.Revision++
	updated.UpdatedAt = updated.UpdatedAt.Add(1)
	if err := store.UpdateRunbookActivation(ctx, updated, activation.Revision); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetRunbookActivation(ctx, scope, activation.ID)
	if err != nil || restored == nil || restored.ObjectiveID != objective.ID || restored.NextRunAt == nil || !restored.NextRunAt.Equal(due) {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
	replayed, err := routine.NewRunbookActivationService(restarted).Create(ctx, routine.CreateRunbookActivationRequest{
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
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}, Title: "Outcome", Goal: "Do useful work", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other"}, ObjectiveID: objective.ID, AssignedAgentID: "other",
		DefinitionID: "work", DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err == nil {
		t.Fatal("cross-owner Runbook activation succeeded")
	}
}

func TestRunbookActivationLifecycleIsRevisionBoundAndRetirementIsFinal(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Outcome", Goal: "Do useful work", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := routine.NewRunbookActivationService(store)
	activation, err := service.Create(ctx, routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "rowan", DefinitionID: "work",
		DefinitionVersion: "1", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := routine.StartRunbookActivation(ctx, store, scope, activation.ID, routine.StartRunbookActivationRequest{IdempotencyKey: "manual-run"})
	if err != nil || started.Run == nil {
		t.Fatalf("started=%#v err=%v", started, err)
	}
	if pin, ok := started.Run.Plan["runbook"].(map[string]interface{}); !ok || pin["id"] != "work" || pin["version"] != "1" || pin["trigger"] != "daily" {
		t.Fatalf("manual Run pin=%#v", started.Run.Plan)
	}
	paused, err := service.Update(ctx, scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: models.RunbookActivationPaused})
	if err != nil || paused.Status != models.RunbookActivationPaused || paused.Revision != 2 {
		t.Fatalf("paused=%#v err=%v", paused, err)
	}
	if _, err = routine.StartRunbookActivation(ctx, store, scope, activation.ID, routine.StartRunbookActivationRequest{IdempotencyKey: "paused-run"}); !errors.Is(err, routine.ErrRunbookActivationInactive) {
		t.Fatalf("paused start error=%v", err)
	}
	if _, err = service.Update(ctx, scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: 1, Status: models.RunbookActivationActive}); !errors.Is(err, models.ErrRunbookActivationRevision) {
		t.Fatalf("stale update error=%v", err)
	}
	retired, err := service.Update(ctx, scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: paused.Revision, Status: models.RunbookActivationRetired})
	if err != nil || retired.Status != models.RunbookActivationRetired {
		t.Fatalf("retired=%#v err=%v", retired, err)
	}
	if _, err = service.Update(ctx, scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: retired.Revision, Status: models.RunbookActivationActive}); err == nil {
		t.Fatal("retired Runbook resumed")
	}
}

func TestAutomaticallyExhaustedScheduleRemainsCallableOnDemand(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "3"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "rowan"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Outcome", Goal: "Do useful work", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := routine.NewRunbookActivationService(store).Create(ctx, routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "work", DefinitionVersion: "1", TriggerID: "hourly",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{
			Cron: "0 0 * * * *", Timezone: "UTC", MaximumOccurrences: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	activation.Status = models.RunbookActivationRetired
	activation.OccurrencesProcessed = 2
	expectedRevision := activation.Revision
	activation.Revision++
	if err := store.UpdateRunbookActivation(ctx, activation, expectedRevision); err != nil {
		t.Fatal(err)
	}
	started, err := routine.StartRunbookActivation(ctx, store, scope, activation.ID, routine.StartRunbookActivationRequest{IdempotencyKey: "off-cycle"})
	if err != nil || started.Run == nil || started.Run.Source != models.RunSourceManual {
		t.Fatalf("off-cycle exhausted start=%#v err=%v", started, err)
	}
}

// eventTaskFixture creates an Objective and a chat with a user request that an
// event Task can report to, and returns a create request for a webhook routine.
func eventTaskFixture(t *testing.T, store *sqlite.Store) (models.Scope, routine.CreateRunbookActivationRequest) {
	t.Helper()
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "event-task"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}
	objective, err := runtimeportfolio.NewPortfolioService(store).CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Order alerts", Goal: "Summarize new orders", Status: models.ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversations := conversation.NewConversationService(store)
	seedTestSpace(t, store, scope)
	c, _, err := conversations.CreateConversation(ctx, conversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Orders", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-1"}, Intent: models.MessageIntentQuestion, Content: "Tell me when an order comes in", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "user-request"})
	if err != nil {
		t.Fatal(err)
	}
	key := "create-task:orders"
	hookID := models.HookID(scope, routine.RunbookActivationIDForKey(scope, key))
	return scope, routine.CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, TriggerID: "event", MaximumConcurrent: 1,
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent}, IdempotencyKey: key,
		Task: &models.ScheduledAgentTask{
			Goal: "Summarize the new order.", ConversationID: c.ID, SourceMessageID: posted.Message.ID, CreatedBy: "user-1",
			Event: &models.RoutineEventTrigger{
				Kind: models.RoutineEventWebhook, SourceRef: models.HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20,
				Webhook: &models.RoutineWebhook{HookID: hookID},
			},
		},
	}
}

func TestRunbookActivationCreateEventTaskNoCursor(t *testing.T) {
	store := kerneltest.NewStore(t)
	scope, request := eventTaskFixture(t, store)
	activation, err := routine.NewRunbookActivationService(store).Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if activation.ID != routine.RunbookActivationIDForKey(scope, request.IdempotencyKey) {
		t.Fatalf("activation ID %q is not the ID for its key", activation.ID)
	}
	if activation.NextOccurrenceBase != nil || activation.NextRunAt != nil || activation.OccurrencesProcessed != 0 {
		t.Fatalf("event task has a schedule cursor: %#v", activation)
	}
	if activation.Task.Event == nil || activation.Task.Event.Webhook == nil || activation.Task.Event.Webhook.HookID != request.Task.Event.Webhook.HookID {
		t.Fatalf("event trigger was not kept: %#v", activation.Task)
	}
	stored, err := store.GetRunbookActivation(t.Context(), scope, activation.ID)
	if err != nil || stored == nil || stored.Task.Event == nil || stored.Task.Event.Kind != models.RoutineEventWebhook {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	replayed, err := routine.NewRunbookActivationService(store).Create(t.Context(), request)
	if err != nil || replayed.ID != activation.ID {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	missingCreator := request
	task := *request.Task
	task.CreatedBy = ""
	missingCreator.Task, missingCreator.IdempotencyKey = &task, "create-task:no-creator"
	if _, err := routine.NewRunbookActivationService(store).Create(t.Context(), missingCreator); err == nil {
		t.Fatal("event task without a creator was created")
	}
}

func TestRunbookActivationPausedReason(t *testing.T) {
	store := kerneltest.NewStore(t)
	scope, request := eventTaskFixture(t, store)
	service := routine.NewRunbookActivationService(store)
	activation, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: models.RunbookActivationActive, PausedReason: models.RoutinePausedNeedsSignIn}); err == nil {
		t.Fatal("paused reason accepted without a pause")
	}
	if _, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: models.RunbookActivationPaused, PausedReason: "tired"}); err == nil {
		t.Fatal("unknown paused reason accepted")
	}
	paused, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: models.RunbookActivationPaused, PausedReason: models.RoutinePausedNeedsSignIn})
	if err != nil || paused.Status != models.RunbookActivationPaused || paused.PausedReason != models.RoutinePausedNeedsSignIn || paused.Revision != activation.Revision+1 {
		t.Fatalf("paused=%#v err=%v", paused, err)
	}
	stored, err := store.GetRunbookActivation(t.Context(), scope, activation.ID)
	if err != nil || stored.PausedReason != models.RoutinePausedNeedsSignIn {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	resumed, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: paused.Revision, Status: models.RunbookActivationActive})
	if err != nil || resumed.Status != models.RunbookActivationActive || resumed.PausedReason != "" {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	if resumed.Task.CreatedBy != "user-1" {
		t.Fatalf("resume changed the creator: %q", resumed.Task.CreatedBy)
	}
}

func TestRunbookActivationPausedReasonOnPausedRoutine(t *testing.T) {
	store := kerneltest.NewStore(t)
	scope, request := eventTaskFixture(t, store)
	service := routine.NewRunbookActivationService(store)
	activation, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	byPerson, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: models.RunbookActivationPaused})
	if err != nil || byPerson.PausedReason != "" {
		t.Fatalf("person pause=%#v err=%v", byPerson, err)
	}
	bySystem, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: byPerson.Revision, Status: models.RunbookActivationPaused, PausedReason: models.RoutinePausedSourceRemoved})
	if err != nil || bySystem.PausedReason != models.RoutinePausedSourceRemoved || bySystem.Revision != byPerson.Revision+1 {
		t.Fatalf("system pause of a paused routine=%#v err=%v", bySystem, err)
	}
	again, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: bySystem.Revision, Status: models.RunbookActivationPaused, PausedReason: models.RoutinePausedSourceRemoved})
	if err != nil || again.Revision != bySystem.Revision {
		t.Fatalf("same reason wrote a revision: %#v err=%v", again, err)
	}
	repaused, err := service.Update(t.Context(), scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: bySystem.Revision, Status: models.RunbookActivationPaused})
	if err != nil || repaused.PausedReason != models.RoutinePausedSourceRemoved || repaused.Revision != bySystem.Revision {
		t.Fatalf("a person pausing a paused routine dropped its reason: %#v err=%v", repaused, err)
	}
}
