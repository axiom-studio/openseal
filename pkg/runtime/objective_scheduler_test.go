package runtime

import (
	"context"
	"path/filepath"
	"strings"
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
	if err := (&ObjectiveCadence{Type: ObjectiveCadenceCron, CronExpression: "*/5 * * * * *"}).Validate(); err != nil {
		t.Fatalf("validate six-field cron cadence: %v", err)
	}
	if err := (&ObjectiveCadence{Type: ObjectiveCadenceCron, CronExpression: "*/5 * * * *"}).Validate(); err == nil {
		t.Fatal("expected five-field cron cadence to fail closed")
	}
}

func TestObjectiveCadenceCronNextUsesConfiguredTimezone(t *testing.T) {
	cadence := &ObjectiveCadence{
		Type: ObjectiveCadenceCron, CronExpression: "0 30 9 * * *", Timezone: "America/New_York",
	}
	next, err := cadence.Next(time.Date(2026, time.January, 2, 14, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("next cron cadence: %v", err)
	}
	want := time.Date(2026, time.January, 3, 14, 30, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestObjectiveCadenceCapabilityRequiresBudgetForBothDurablePhases(t *testing.T) {
	template := &ObjectiveRunTemplate{Capability: &ObjectiveCapabilityInvocation{
		SkillID: "openseal.kubernetes", SkillVersion: "1.0.1", Action: "list_events",
	}}
	for name, budget := range map[string]*BudgetPolicy{
		"one attempt": {MaxAttempts: 1, MaxTurns: 2},
		"one turn":    {MaxAttempts: 2, MaxTurns: 1},
	} {
		t.Run(name, func(t *testing.T) {
			cadence := &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 60, RunBudget: budget, RunTemplate: template}
			if err := cadence.Validate(); err == nil || !strings.Contains(err.Error(), "at least 2") {
				t.Fatalf("non-completable capability budget accepted: %v", err)
			}
		})
	}
	for name, budget := range map[string]*BudgetPolicy{
		"unbounded": {},
		"bounded":   {MaxAttempts: 2, MaxTurns: 2},
	} {
		t.Run(name, func(t *testing.T) {
			cadence := &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 60, RunBudget: budget, RunTemplate: template}
			if err := cadence.Validate(); err != nil {
				t.Fatalf("completable capability budget rejected: %v", err)
			}
		})
	}
}

func TestObjectiveSchedulerCreatesCanonicalBoundedRunAndBackpressures(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	due := now.Add(-time.Minute)
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "scheduler"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"},
		Title: "Operate", Goal: "Inspect production health", Status: ObjectiveStatusActive,
		Budget: &BudgetPolicy{MaxTurns: 10}, Cadence: &ObjectiveCadence{
			Type: ObjectiveCadenceInterval, IntervalSeconds: 300, AssignedAgentID: "sre",
			RunBudget: &BudgetPolicy{MaxTurns: 2}, RunTemplate: &ObjectiveRunTemplate{
				Context: map[string]interface{}{
					"initiativeId": "initiative-health", "sourceMonitorId": "cluster-events",
				}, Policy: map[string]interface{}{"sourcePolicyRef": "production-read-only"}, Capability: &ObjectiveCapabilityInvocation{
					SkillID: "kubernetes-events", SkillVersion: "1.0.0", Action: "watch", Inputs: map[string]interface{}{"namespace": "production"},
				},
			},
		},
		NextEvaluationAt: &due, IdempotencyKey: "operate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = NewInitiativeService(store, store).Create(ctx, CreateInitiativeRequest{Initiative: &Initiative{
		ID: "initiative-health", Scope: objective.Scope, Owner: objective.Owner, Title: "Production health", Purpose: "Coordinate production monitoring",
		Status: InitiativeStatusActive, ObjectiveRefs: []string{objective.ID}, SourceMonitors: []SourceMonitorReference{{
			ID: "cluster-events", ObjectiveID: objective.ID, AssignedAgentID: "sre", SkillID: "kubernetes-events", SkillVersion: "1.0.0", Action: "watch",
			SourcePolicyRef: "production-read-only", Deduplication: SourceMonitorDeduplicateStableSourceAndContent,
		}},
	}, Actor: ActivityActor{Type: "user", ID: "operator"}}); err != nil {
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
	if runs[0].Source != RunSourceSchedule || runs[0].Budget == nil || runs[0].Budget.MaxTurns != 2 || runs[0].AssignedAgentID != "sre" ||
		runs[0].Entrypoint != "" || runs[0].Context["initiativeId"] != "initiative-health" || runs[0].Context["sourceMonitorId"] != "cluster-events" ||
		runs[0].Context["scheduledFor"] != due.Format(time.RFC3339Nano) || runs[0].Policy["sourcePolicyRef"] != "production-read-only" {
		t.Fatalf("run = %#v", runs[0])
	}
	invocation, ok := runs[0].Context["capabilityInvocation"].(map[string]interface{})
	if !ok || invocation["skillId"] != "kubernetes-events" || invocation["skillVersion"] != "1.0.0" || invocation["action"] != "watch" || invocation["inputs"].(map[string]interface{})["namespace"] != "production" {
		t.Fatalf("scheduled capability invocation = %#v", runs[0].Context["capabilityInvocation"])
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
	backpressured, err := store.GetObjective(ctx, objective.Scope, objective.ID)
	if err != nil || backpressured.ScheduleCondition == nil || backpressured.ScheduleCondition.State != ObjectiveScheduleBackpressured ||
		backpressured.NextEvaluationAt == nil || !backpressured.NextEvaluationAt.Equal(scheduler.now().Add(5*time.Minute)) {
		t.Fatalf("backpressured objective = %#v, err = %v", backpressured, err)
	}
	running := AgentRunStatusRunning
	activeRun, _, err := NewRunActivityService(store, store).TransitionRun(ctx, objective.Scope, runs[0].ID, RunTransitionRequest{ExpectedRevision: runs[0].Revision, Status: running})
	if err != nil {
		t.Fatal(err)
	}
	completed := AgentRunStatusCompleted
	if _, _, err = NewRunActivityService(store, store).TransitionRun(ctx, objective.Scope, activeRun.ID, RunTransitionRequest{ExpectedRevision: activeRun.Revision, Status: completed}); err != nil {
		t.Fatal(err)
	}
	scheduler.now = func() time.Time { return backpressured.NextEvaluationAt.Add(time.Second) }
	resumed, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || resumed.Scheduled != 1 {
		t.Fatalf("resumed reconciliation = %#v, err = %v", resumed, err)
	}
	cleared, err := store.GetObjective(ctx, objective.Scope, objective.ID)
	if err != nil || cleared.ScheduleCondition != nil {
		t.Fatalf("cleared schedule condition = %#v, err = %v", cleared, err)
	}
}

func TestObjectiveSchedulerDoesNotInventTeamAssignee(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 11, 5, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	due := now.Add(-time.Minute)
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "scheduler-team"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "sre-team"},
		Title: "Coordinate", Goal: "Review production health", Status: ObjectiveStatusActive,
		Cadence:          &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 300},
		NextEvaluationAt: &due, IdempotencyKey: "coordinate",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewObjectiveScheduler(store)
	scheduler.now = func() time.Time { return now }
	result, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || result.Scheduled != 1 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(runs) != 1 || runs[0].AssignedAgentID != "" {
		t.Fatalf("team runs = %#v, err = %v", runs, err)
	}
}

func TestObjectiveSchedulerDefaultsAgentAssigneeToOwner(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 11, 5, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	due := now.Add(-time.Minute)
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "scheduler-agent"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre-agent"},
		Title: "Operate", Goal: "Inspect production health", Status: ObjectiveStatusActive,
		Cadence:          &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 300},
		NextEvaluationAt: &due, IdempotencyKey: "operate-default-assignee",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewObjectiveScheduler(store)
	scheduler.now = func() time.Time { return now }
	result, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || result.Scheduled != 1 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(runs) != 1 || runs[0].AssignedAgentID != objective.Owner.ID {
		t.Fatalf("agent runs = %#v, err = %v", runs, err)
	}
}

func TestObjectiveSchedulerDefersPausedInitiativeMonitor(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 11, 6, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "paused-monitor"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"},
		Title: "Monitor sources", Goal: "Collect evidence", Status: ObjectiveStatusActive, NextEvaluationAt: &due,
		Cadence: &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 300, AssignedAgentID: "analyst", RunTemplate: &ObjectiveRunTemplate{
			Context: map[string]interface{}{"initiativeId": "initiative-paused", "sourceMonitorId": "forum"},
			Policy:  map[string]interface{}{"sourcePolicyRef": "approved@1"}, Capability: &ObjectiveCapabilityInvocation{SkillID: "source", SkillVersion: "1", Action: "observe"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = NewInitiativeService(store, store).Create(ctx, CreateInitiativeRequest{Initiative: &Initiative{
		ID: "initiative-paused", Scope: objective.Scope, Owner: objective.Owner, Title: "Research", Purpose: "Collect evidence", Status: InitiativeStatusPaused,
		ObjectiveRefs: []string{objective.ID}, SourceMonitors: []SourceMonitorReference{{ID: "forum", ObjectiveID: objective.ID, AssignedAgentID: "analyst", SkillID: "source", SkillVersion: "1", Action: "observe", SourcePolicyRef: "approved@1", Deduplication: SourceMonitorDeduplicateStableSourceAndContent}},
	}, Actor: ActivityActor{Type: "user", ID: "operator"}}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewObjectiveScheduler(store)
	scheduler.now = func() time.Time { return now }
	result, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || result.Suspended != 1 || result.Scheduled != 0 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs = %#v, err = %v", runs, err)
	}
	loaded, err := store.GetObjective(ctx, objective.Scope, objective.ID)
	if err != nil || loaded.NextEvaluationAt == nil || !loaded.NextEvaluationAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("deferred objective = %#v, err = %v", loaded, err)
	}
	if loaded.ScheduleCondition == nil || loaded.ScheduleCondition.State != ObjectiveScheduleSuspended {
		t.Fatalf("suspended schedule condition = %#v", loaded.ScheduleCondition)
	}
}

func TestObjectiveSchedulerPersistsBudgetExhaustionWithoutFailingScope(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: Scope{Kind: "tenant", ID: "budgeted-monitor"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"},
		Title: "Bounded research", Goal: "Collect permitted evidence", Status: ObjectiveStatusActive,
		Budget: &BudgetPolicy{MaxTurns: 1}, NextEvaluationAt: &due,
		Cadence: &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 60, AssignedAgentID: "analyst", RunBudget: &BudgetPolicy{MaxTurns: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewObjectiveScheduler(store)
	scheduler.now = func() time.Time { return now }
	first, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || first.Scheduled != 1 {
		t.Fatalf("first reconciliation = %#v, %v", first, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: objective.Scope, ObjectiveID: objective.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %#v, %v", runs, err)
	}
	running := AgentRunStatusRunning
	active, _, err := NewRunActivityService(store, store).TransitionRun(ctx, objective.Scope, runs[0].ID, RunTransitionRequest{ExpectedRevision: runs[0].Revision, Status: running})
	if err != nil {
		t.Fatal(err)
	}
	completed := AgentRunStatusCompleted
	if _, _, err = NewRunActivityService(store, store).TransitionRun(ctx, objective.Scope, runs[0].ID, RunTransitionRequest{ExpectedRevision: active.Revision, Status: completed}); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetObjective(ctx, objective.Scope, objective.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.now = func() time.Time { return current.NextEvaluationAt.Add(time.Second) }
	second, err := scheduler.ReconcileScope(ctx, objective.Scope, 10)
	if err != nil || second.BudgetExhausted != 1 || second.Scheduled != 0 {
		t.Fatalf("budget reconciliation = %#v, %v", second, err)
	}
	exhausted, err := store.GetObjective(ctx, objective.Scope, objective.ID)
	if err != nil || exhausted.ScheduleCondition == nil || exhausted.ScheduleCondition.State != ObjectiveScheduleBudgetExhausted || exhausted.Status != ObjectiveStatusActive {
		t.Fatalf("exhausted objective = %#v, %v", exhausted, err)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: objective.Scope, ObjectiveID: objective.ID, Descending: true, Limit: 20})
	if err != nil || len(events) == 0 || events[0].TeamID != "research" || events[0].Visibility != ActivityVisibilityTeam {
		t.Fatalf("budget activity = %#v, %v", events, err)
	}
}

func TestObjectiveCadenceRunTemplateFailsClosedOnSecretsAndReservedContext(t *testing.T) {
	for name, template := range map[string]*ObjectiveRunTemplate{
		"secret context":    {Context: map[string]interface{}{"apiKey": "must-not-persist"}},
		"secret policy":     {Policy: map[string]interface{}{"apiToken": "must-not-persist"}},
		"scheduledFor":      {Context: map[string]interface{}{"scheduledFor": "invented"}},
		"capability field":  {Context: map[string]interface{}{"capabilityInvocation": "invented"}},
		"evidence snapshot": {Context: map[string]interface{}{EvidenceSnapshotContextKey: map[string]interface{}{"id": "invented"}}},
		"capability secret": {Capability: &ObjectiveCapabilityInvocation{SkillID: "reader", SkillVersion: "1", Action: "search", Inputs: map[string]interface{}{"apiKey": "must-not-persist"}}},
		"evidence bounds":   {EvidenceProjection: &ObjectiveEvidenceProjection{MaximumObservations: 100}},
		"long entrypoint":   {Entrypoint: string(make([]byte, 129))},
		"ambiguous operation": {
			Entrypoint: "monitor",
			Capability: &ObjectiveCapabilityInvocation{SkillID: "reader", SkillVersion: "1", Action: "search"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cadence := &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 60, RunTemplate: template}
			if err := cadence.Validate(); err == nil {
				t.Fatal("unsafe run template was accepted")
			}
		})
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
