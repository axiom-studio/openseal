package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestRunbookActivationIdempotentCreatesConvergeAcrossRace(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
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
	store := newTestKernelStore(t)
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

func TestResolvedVerificationBlocksManualAndScheduledActivation(t *testing.T) {
	ctx := t.Context()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "verification.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := Scope{Kind: "tenant", ID: "verification"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "operator"}
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
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Operate", Goal: "Operate safely", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "hourly", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: definition.Runbook.ID, DefinitionVersion: definition.Runbook.Version, TriggerID: "hourly", Trigger: definition.Runbook.Triggers["hourly"],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartRunbookActivation(ctx, store, scope, activation.ID, StartRunbookActivationRequest{}); !errors.Is(err, ErrRunbookActivationUnverified) {
		t.Fatalf("manual activation error = %v", err)
	}
	result, err := NewRunbookScheduler(store).ReconcileScope(ctx, scope, 10)
	if err != nil || result.Suspended != 1 || result.Examined != 1 {
		t.Fatalf("schedule verification = %#v, %v", result, err)
	}
	paused, err := store.GetRunbookActivation(ctx, scope, activation.ID)
	if err != nil || paused.Status != RunbookActivationPaused {
		t.Fatalf("paused activation = %#v, %v", paused, err)
	}
}

func TestRunbookActivationLivesUnderObjectiveAndOwnsSchedule(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
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
	store := newTestKernelStore(t)
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
	seedTestSpace(t, store, scope)
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
	store := newTestKernelStore(t)
	scope := Scope{Kind: "tenant", ID: "3"}
	seedTestSpace(t, store, scope)
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
	store := newTestKernelStore(t)
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
	store := newTestKernelStore(t)
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

func TestAutomaticallyExhaustedScheduleRemainsCallableOnDemand(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := Scope{Kind: "tenant", ID: "3"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Outcome", Goal: "Do useful work", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "work", DefinitionVersion: "1", TriggerID: "hourly",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: &runbook.Schedule{
			Cron: "0 0 * * * *", Timezone: "UTC", MaximumOccurrences: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	activation.Status = RunbookActivationRetired
	activation.OccurrencesProcessed = 2
	expectedRevision := activation.Revision
	activation.Revision++
	if err := store.UpdateRunbookActivation(ctx, activation, expectedRevision); err != nil {
		t.Fatal(err)
	}
	started, err := StartRunbookActivation(ctx, store, scope, activation.ID, StartRunbookActivationRequest{IdempotencyKey: "off-cycle"})
	if err != nil || started.Run == nil || started.Run.Source != RunSourceManual {
		t.Fatalf("off-cycle exhausted start=%#v err=%v", started, err)
	}
}

// eventTaskFixture creates an Objective and a chat with a user request that an
// event Task can report to, and returns a create request for a webhook routine.
func eventTaskFixture(t *testing.T, store *SQLiteStore) (Scope, CreateRunbookActivationRequest) {
	t.Helper()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "event-task"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Order alerts", Goal: "Summarize new orders", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversations := NewConversationService(store)
	seedTestSpace(t, store, scope)
	c, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Orders", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentQuestion, Content: "Tell me when an order comes in", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "user-request"})
	if err != nil {
		t.Fatal(err)
	}
	key := "create-task:orders"
	hookID := HookID(scope, RunbookActivationIDForKey(scope, key))
	return scope, CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, TriggerID: "event", MaximumConcurrent: 1,
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent}, IdempotencyKey: key,
		Task: &ScheduledAgentTask{
			Goal: "Summarize the new order.", ConversationID: c.ID, SourceMessageID: posted.Message.ID, CreatedBy: "user-1",
			Event: &RoutineEventTrigger{
				Kind: RoutineEventWebhook, SourceRef: HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20,
				Webhook: &RoutineWebhook{HookID: hookID},
			},
		},
	}
}

func eventTaskActivationForValidation() *RunbookActivation {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	scope := Scope{Kind: "tenant", ID: "event-task"}
	hookID := HookID(scope, "activation-1")
	return &RunbookActivation{
		ID: "activation-1", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, ObjectiveID: "objective-1",
		AssignedAgentID: "researcher", TriggerID: "event", Trigger: runbook.Trigger{Kind: runbook.TriggerEvent},
		Task: &ScheduledAgentTask{
			Goal: "Summarize the new order.", ConversationID: "conversation-1", SourceMessageID: "message-1", CreatedBy: "user-1",
			Event: &RoutineEventTrigger{
				Kind: RoutineEventWebhook, SourceRef: HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20,
				Webhook: &RoutineWebhook{HookID: hookID},
			},
		},
		Status: RunbookActivationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func TestRunbookActivationEventTaskValidation(t *testing.T) {
	schedule := &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}
	cases := []struct {
		name   string
		change func(*RunbookActivation)
		valid  bool
	}{
		{"event task", func(*RunbookActivation) {}, true},
		{"event task without creator", func(a *RunbookActivation) { a.Task.CreatedBy = "" }, false},
		{"event kind without event trigger", func(a *RunbookActivation) { a.Task.Event = nil }, false},
		{"event task with schedule", func(a *RunbookActivation) { a.Trigger.Schedule = schedule }, false},
		{"event task with entrypoint", func(a *RunbookActivation) { a.Trigger.Entrypoint = "run" }, false},
		{"event task with trigger reporting", func(a *RunbookActivation) {
			a.Trigger.Reporting = &runbook.ReportingPolicy{Channel: "orders", Title: "Orders"}
		}, false},
		{"event task with invalid event trigger", func(a *RunbookActivation) { a.Task.Event.MaxPerHour = 0 }, false},
		{"event task with cursor", func(a *RunbookActivation) {
			now := a.CreatedAt
			a.NextOccurrenceBase, a.NextRunAt = &now, &now
		}, false},
		{"event task with occurrence progress", func(a *RunbookActivation) { a.OccurrencesProcessed = 1 }, false},
		{"schedule task with event trigger", func(a *RunbookActivation) {
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: schedule}
		}, false},
		{"schedule task", func(a *RunbookActivation) {
			a.Task.Event = nil
			a.Task.CreatedBy = ""
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: schedule}
		}, true},
		{"definition schedule", func(a *RunbookActivation) {
			a.Task = nil
			a.DefinitionID, a.DefinitionVersion = "work", "1"
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: schedule}
		}, true},
		{"definition event activation", func(a *RunbookActivation) {
			a.Task = nil
			a.DefinitionID, a.DefinitionVersion = "work", "1"
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerEvent, Entrypoint: "run"}
		}, false},
		{"paused with reason", func(a *RunbookActivation) {
			a.Status, a.PausedReason = RunbookActivationPaused, RoutinePausedNeedsSignIn
		}, true},
		{"active with reason", func(a *RunbookActivation) { a.PausedReason = RoutinePausedSourceRemoved }, false},
		{"paused with unknown reason", func(a *RunbookActivation) {
			a.Status, a.PausedReason = RunbookActivationPaused, "tired"
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activation := eventTaskActivationForValidation()
			tc.change(activation)
			if err := activation.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	retired := eventTaskActivationForValidation()
	retired.Status = RunbookActivationRetired
	if retired.Callable() {
		t.Fatal("an event task the person retired is callable")
	}
	retired.Exhausted = true
	if !retired.Callable() || retired.Validate() != nil {
		t.Fatal("an exhausted event task is not callable on demand")
	}
	retired.Status = RunbookActivationPaused
	if retired.Callable() || retired.Validate() == nil {
		t.Fatal("a paused event task is callable or exhausted")
	}
}

func TestRunbookActivationCreateEventTaskNoCursor(t *testing.T) {
	store := newTestKernelStore(t)
	scope, request := eventTaskFixture(t, store)
	activation, err := NewRunbookActivationService(store).Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if activation.ID != RunbookActivationIDForKey(scope, request.IdempotencyKey) {
		t.Fatalf("activation ID %q is not the ID for its key", activation.ID)
	}
	if activation.NextOccurrenceBase != nil || activation.NextRunAt != nil || activation.OccurrencesProcessed != 0 {
		t.Fatalf("event task has a schedule cursor: %#v", activation)
	}
	if activation.Task.Event == nil || activation.Task.Event.Webhook == nil || activation.Task.Event.Webhook.HookID != request.Task.Event.Webhook.HookID {
		t.Fatalf("event trigger was not kept: %#v", activation.Task)
	}
	stored, err := store.GetRunbookActivation(t.Context(), scope, activation.ID)
	if err != nil || stored == nil || stored.Task.Event == nil || stored.Task.Event.Kind != RoutineEventWebhook {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	replayed, err := NewRunbookActivationService(store).Create(t.Context(), request)
	if err != nil || replayed.ID != activation.ID {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	missingCreator := request
	task := *request.Task
	task.CreatedBy = ""
	missingCreator.Task, missingCreator.IdempotencyKey = &task, "create-task:no-creator"
	if _, err := NewRunbookActivationService(store).Create(t.Context(), missingCreator); err == nil {
		t.Fatal("event task without a creator was created")
	}
}

func TestRunbookActivationPausedReason(t *testing.T) {
	store := newTestKernelStore(t)
	scope, request := eventTaskFixture(t, store)
	service := NewRunbookActivationService(store)
	activation, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: RunbookActivationActive, PausedReason: RoutinePausedNeedsSignIn}); err == nil {
		t.Fatal("paused reason accepted without a pause")
	}
	if _, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: RunbookActivationPaused, PausedReason: "tired"}); err == nil {
		t.Fatal("unknown paused reason accepted")
	}
	paused, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: RunbookActivationPaused, PausedReason: RoutinePausedNeedsSignIn})
	if err != nil || paused.Status != RunbookActivationPaused || paused.PausedReason != RoutinePausedNeedsSignIn || paused.Revision != activation.Revision+1 {
		t.Fatalf("paused=%#v err=%v", paused, err)
	}
	stored, err := store.GetRunbookActivation(t.Context(), scope, activation.ID)
	if err != nil || stored.PausedReason != RoutinePausedNeedsSignIn {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	resumed, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: paused.Revision, Status: RunbookActivationActive})
	if err != nil || resumed.Status != RunbookActivationActive || resumed.PausedReason != "" {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	if resumed.Task.CreatedBy != "user-1" {
		t.Fatalf("resume changed the creator: %q", resumed.Task.CreatedBy)
	}
}

func TestRunbookActivationPausedReasonOnPausedRoutine(t *testing.T) {
	store := newTestKernelStore(t)
	scope, request := eventTaskFixture(t, store)
	service := NewRunbookActivationService(store)
	activation, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	byPerson, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: activation.Revision, Status: RunbookActivationPaused})
	if err != nil || byPerson.PausedReason != "" {
		t.Fatalf("person pause=%#v err=%v", byPerson, err)
	}
	bySystem, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: byPerson.Revision, Status: RunbookActivationPaused, PausedReason: RoutinePausedSourceRemoved})
	if err != nil || bySystem.PausedReason != RoutinePausedSourceRemoved || bySystem.Revision != byPerson.Revision+1 {
		t.Fatalf("system pause of a paused routine=%#v err=%v", bySystem, err)
	}
	again, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: bySystem.Revision, Status: RunbookActivationPaused, PausedReason: RoutinePausedSourceRemoved})
	if err != nil || again.Revision != bySystem.Revision {
		t.Fatalf("same reason wrote a revision: %#v err=%v", again, err)
	}
	repaused, err := service.Update(t.Context(), scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: bySystem.Revision, Status: RunbookActivationPaused})
	if err != nil || repaused.PausedReason != RoutinePausedSourceRemoved || repaused.Revision != bySystem.Revision {
		t.Fatalf("a person pausing a paused routine dropped its reason: %#v err=%v", repaused, err)
	}
}
