package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestRunbookSchedulerRetiresAfterExactMaximumOccurrencesAcrossRestartStores(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "bounded-schedule.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := t.Context()
			store, cleanup := testCase.open(t)
			defer cleanup()
			scope := Scope{Kind: "tenant", ID: "bounded"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "rowan"}
			objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Five useful comments", Goal: "Publish one useful comment per hour for five hours", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
			service := NewRunbookActivationService(store)
			service.now = func() time.Time { return now }
			activation, err := service.Create(ctx, CreateRunbookActivationRequest{
				ID: "five-hour-acceptance", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
				DefinitionID: "hourly-comment", DefinitionVersion: "1", TriggerID: "hourly", MaximumConcurrent: 10,
				Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "comment", Schedule: &runbook.Schedule{
					Cron: "0 0 * * * *", Timezone: "UTC", MaximumOccurrences: 5,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			scheduler := NewRunbookScheduler(store)
			scheduler.now = func() time.Time { return now }
			if result, err := scheduler.ReconcileScope(ctx, scope, 10); err != nil || result.Initialized != 1 {
				t.Fatalf("initialize = %#v, %v", result, err)
			}
			cursor, _ := store.GetRunbookActivation(ctx, scope, activation.ID)
			now = cursor.NextRunAt.Add(time.Second)
			if result, err := scheduler.ReconcileScope(ctx, scope, 10); err != nil || result.Scheduled != 1 {
				t.Fatalf("first occurrence = %#v, %v", result, err)
			}

			// Simulate a crash after durable Run creation but before durable cursor
			// advancement. Reconciliation must replay the same occurrence, count it
			// once, and never create another Run.
			advanced, _ := store.GetRunbookActivation(ctx, scope, activation.ID)
			replay := cloneRunbookActivation(advanced)
			replay.NextOccurrenceBase, replay.NextRunAt = cursor.NextOccurrenceBase, cursor.NextRunAt
			replay.OccurrencesProcessed = 0
			replay.Revision++
			replay.UpdatedAt = now
			if err := store.UpdateRunbookActivation(ctx, replay, advanced.Revision); err != nil {
				t.Fatal(err)
			}
			if result, err := scheduler.ReconcileScope(ctx, scope, 10); err != nil || result.Replayed != 1 || result.Scheduled != 0 {
				t.Fatalf("replayed occurrence = %#v, %v", result, err)
			}

			for occurrence := int64(2); occurrence <= 5; occurrence++ {
				current, _ := store.GetRunbookActivation(ctx, scope, activation.ID)
				now = current.NextRunAt.Add(time.Second)
				result, err := scheduler.ReconcileScope(ctx, scope, 10)
				if err != nil || result.Scheduled != 1 || occurrence == 5 && result.Retired != 1 {
					t.Fatalf("occurrence %d = %#v, %v", occurrence, result, err)
				}
			}
			terminal, _ := store.GetRunbookActivation(ctx, scope, activation.ID)
			if terminal.Status != RunbookActivationRetired || terminal.OccurrencesProcessed != 5 || terminal.NextRunAt != nil || terminal.NextOccurrenceBase != nil {
				t.Fatalf("terminal activation = %#v", terminal)
			}
			if result, err := scheduler.ReconcileScope(ctx, scope, 10); err != nil || result.Examined != 0 || result.Scheduled != 0 {
				t.Fatalf("post-retirement reconciliation = %#v, %v", result, err)
			}
			runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 10})
			if err != nil || len(runs) != 5 {
				t.Fatalf("scheduled runs = %d, %v", len(runs), err)
			}
		})
	}
}

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
		}, Reporting: &runbook.ReportingPolicy{
			Channel: "community-work", Title: "Community work",
			Milestones: []runbook.ReportingMilestone{runbook.ReportingStarted, runbook.ReportingCompleted, runbook.ReportingFailed},
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
	if pin, ok := created.Plan["runbook"].(map[string]interface{}); !ok || pin["id"] != activation.DefinitionID || pin["version"] != activation.DefinitionVersion || pin["trigger"] != activation.TriggerID {
		t.Fatalf("scheduled Run pin=%#v", created.Plan)
	}
	if communities, ok := created.Context["communities"].([]interface{}); !ok || len(communities) != 1 {
		t.Fatalf("Runbook input=%#v", created.Context)
	}
	conversations, err := store.ListConversations(ctx, ConversationFilter{Scope: scope, Owner: &owner, Limit: 10})
	if err != nil || len(conversations) != 1 || conversations[0].Title != "Community work" {
		t.Fatalf("reporting conversations=%#v err=%v", conversations, err)
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversations[0].ID, Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].Content != "Started: Contribute useful advice" || len(messages[0].References) != 1 || messages[0].References[0].ID != created.ID {
		t.Fatalf("reporting messages=%#v err=%v", messages, err)
	}
	if err := projectRunReportingStartForRun(ctx, store, created); err != nil {
		t.Fatal(err)
	}
	messages, _ = store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversations[0].ID, Limit: 10})
	if len(messages) != 1 {
		t.Fatalf("replayed start duplicated reporting messages=%#v", messages)
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

func TestRunReportingProjectsTerminalMilestoneOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "reporting"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"}
	run := &AgentRun{
		ID: "run-one", Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Publish the weekly brief",
		Status: AgentRunStatusCompleted, Context: map[string]interface{}{},
	}
	policy := &runbook.ReportingPolicy{
		Channel: "work", Title: "Work",
		Milestones: []runbook.ReportingMilestone{runbook.ReportingStarted, runbook.ReportingCompleted},
	}
	channel, startKey, err := prepareRunReporting(ctx, store, scope, owner, policy, run.ID, run.Context)
	if err != nil {
		t.Fatal(err)
	}
	if err := projectRunReportingStart(ctx, store, channel, startKey, run); err != nil {
		t.Fatal(err)
	}
	if err := projectTerminalRunReporting(ctx, store, run); err != nil {
		t.Fatal(err)
	}
	if err := projectTerminalRunReporting(ctx, store, run); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: channel.ID, Limit: 10})
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	if messages[1].Content != "Completed: Publish the weekly brief" || messages[1].ReplyToMessageID != messages[0].ID || !messages[1].BroadcastToChannel {
		t.Fatalf("terminal message=%#v", messages[1])
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

func TestRunbookSchedulerCapturesBoundedProjectEvidence(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "evidence"}
	project, monitorRuns := seedExecutableMonitorProject(t, store, scope)
	monitorService := NewSourceMonitorService(store, store, store, nil)
	if _, err := monitorService.Ingest(ctx, sourceObservationRequest(scope, project.ID, "monitor-a", monitorRuns["monitor-a"], 0, "one", "thread-one", "a deliberately long finding")); err != nil {
		t.Fatal(err)
	}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: project.Owner, Title: "Synthesize", Goal: "Synthesize cited findings", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "synthesize", Scope: scope, Owner: project.Owner, ObjectiveID: objective.ID, AssignedAgentID: "researcher",
		DefinitionID: "research", DefinitionVersion: "1", TriggerID: "synthesize",
		Trigger: runbook.Trigger{
			Kind: runbook.TriggerSchedule, Entrypoint: "synthesize", Schedule: &runbook.Schedule{Cron: "*/1 * * * * *", Timezone: "UTC"},
			Evidence: &runbook.EvidenceProjection{MaximumObservations: 1, MaximumSummaryRunes: 8, MaximumTotalRunes: 8},
		},
		Input: map[string]interface{}{"projectId": project.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	activation.NextOccurrenceBase, activation.NextRunAt = &due, &due
	activation.Revision++
	activation.UpdatedAt = time.Now().UTC()
	if err := store.UpdateRunbookActivation(ctx, activation, activation.Revision-1); err != nil {
		t.Fatal(err)
	}
	result, err := NewRunbookScheduler(store).ReconcileScope(ctx, scope, 10)
	if err != nil || result.Scheduled != 1 {
		t.Fatalf("schedule=%#v err=%v", result, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	encoded, _ := json.Marshal(runs[0].Context[EvidenceSnapshotContextKey])
	var snapshot EvidenceSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ProjectID != project.ID || snapshot.SelectedCount != 1 || len([]rune(snapshot.Observations[0].Summary)) != 8 || !snapshot.Truncated {
		t.Fatalf("snapshot=%#v", &snapshot)
	}
}
