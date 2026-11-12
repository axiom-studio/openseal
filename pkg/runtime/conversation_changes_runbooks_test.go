package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	runtimeroutine "github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func runbookIDs(runbooks []*conversation.ConversationRunbook) string {
	ids := make([]string, len(runbooks))
	for index, value := range runbooks {
		ids[index] = value.ID
	}
	return strings.Join(ids, ",")
}

func TestConversationChangesCarryLinkedRunbooks(t *testing.T) {
	for _, testCase := range interactionChangeStores() {
		t.Run(testCase.name, func(t *testing.T) {
			f := newInteractionChangeFixture(t, testCase.store(t))
			ctx, store, scope := f.ctx, f.store, f.scope
			owner := f.conversation.Owner
			conversations := conversation.NewConversationService(store.(ConversationStore))
			objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Morning news", Goal: "Summarise the news every morning", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
			activations := runtimeroutine.NewRunbookActivationService(store, models.WithClock(func() time.Time { return now }))
			daily, err := activations.Create(ctx, runtimeroutine.CreateRunbookActivationRequest{
				ID: "daily-news", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
				DefinitionID: "news", DefinitionVersion: "1", TriggerID: "daily", MaximumConcurrent: 1,
				Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "summarise", Schedule: &runbook.Schedule{Cron: "0 0 7 * * *", Timezone: "Europe/London"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			// A scheduled task made from this chat is already linked to it.
			task := &RunbookActivation{
				ID: "chat-task", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, TriggerID: "weekly",
				Task: &ScheduledAgentTask{Goal: "Send the weekly plan", ConversationID: f.conversation.ID, SourceMessageID: f.trigger.ID,
					Script: &RoutineScript{Path: "scripts/plan.py", Digest: routineScriptDigestOf("a"), FilesVersion: 1, Command: "python3 scripts/plan.py", TimeoutSeconds: 60}},
				Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 8 * * 1", Timezone: "UTC"}},
				Status:  RunbookActivationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := store.CreateRunbookActivation(ctx, task); err != nil {
				t.Fatal(err)
			}
			// A scheduled task from another chat is not.
			other := *task
			other.ID, other.Task = "other-task", &ScheduledAgentTask{Goal: "Elsewhere", ConversationID: "other-chat", SourceMessageID: "m"}
			if err := store.CreateRunbookActivation(ctx, &other); err != nil {
				t.Fatal(err)
			}

			initial := f.list(t, "")
			if !initial.RunbooksChanged || runbookIDs(initial.Runbooks) != "chat-task" {
				t.Fatalf("initial runbooks = %s changed=%v", runbookIDs(initial.Runbooks), initial.RunbooksChanged)
			}
			if got := initial.Runbooks[0]; got.Title != "Morning news" || got.Status != conversation.ConversationRunbookActive || got.Schedule == nil || got.Schedule.Cron != "0 0 8 * * 1" ||
				got.Script == nil || got.Script.Path != "scripts/plan.py" {
				t.Fatalf("task runbook = %#v", got)
			}
			quiet := f.list(t, initial.Cursor)
			if quiet.RunbooksChanged || len(quiet.Runbooks) != 0 || quiet.Runbooks == nil {
				t.Fatalf("unchanged runbooks = %#v changed=%v", quiet.Runbooks, quiet.RunbooksChanged)
			}

			if _, err := conversations.LinkRunbooks(ctx, scope, f.conversation.ID, []string{daily.ID, "deleted-routine"}); err != nil {
				t.Fatal(err)
			}
			linked := f.list(t, quiet.Cursor)
			if !linked.RunbooksChanged || !linked.HasChanges || runbookIDs(linked.Runbooks) != "chat-task,daily-news" {
				t.Fatalf("linked runbooks = %s changed=%v", runbookIDs(linked.Runbooks), linked.RunbooksChanged)
			}
			if got := linked.Runbooks[1]; got.ObjectiveID != objective.ID || got.Schedule.Timezone != "Europe/London" || got.NextRunAt != nil || got.LastRun != nil || got.Script != nil {
				t.Fatalf("daily runbook = %#v", got)
			}

			// The scheduler sets the next run and then starts it.
			scheduler := runtimeroutine.NewRunbookScheduler(store, models.WithClock(func() time.Time { return now }))
			if _, err := scheduler.ReconcileScope(ctx, scope, 10); err != nil {
				t.Fatal(err)
			}
			scheduled := f.list(t, linked.Cursor)
			if !scheduled.RunbooksChanged || scheduled.Runbooks[1].NextRunAt == nil {
				t.Fatalf("next run not projected: %#v", scheduled.Runbooks)
			}
			current, _ := store.GetRunbookActivation(ctx, scope, daily.ID)
			now = current.NextRunAt.Add(time.Second)
			if result, err := scheduler.ReconcileScope(ctx, scope, 10); err != nil || result.Scheduled != 1 {
				t.Fatalf("schedule = %#v, %v", result, err)
			}
			ran := f.list(t, scheduled.Cursor)
			if !ran.RunbooksChanged || ran.Runbooks[1].LastRun == nil || ran.Runbooks[1].LastRun.Status == "" {
				t.Fatalf("last run not projected: %#v", ran.Runbooks)
			}

			current, _ = store.GetRunbookActivation(ctx, scope, daily.ID)
			if _, err := activations.Update(ctx, scope, daily.ID, runtimeroutine.UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: RunbookActivationPaused}); err != nil {
				t.Fatal(err)
			}
			paused := f.list(t, ran.Cursor)
			if !paused.RunbooksChanged || paused.Runbooks[1].Status != conversation.ConversationRunbookPaused || paused.Runbooks[1].NextRunAt != nil {
				t.Fatalf("paused runbook = %#v", paused.Runbooks)
			}
			current, _ = store.GetRunbookActivation(ctx, scope, daily.ID)
			if _, err := activations.Update(ctx, scope, daily.ID, runtimeroutine.UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: RunbookActivationRetired}); err != nil {
				t.Fatal(err)
			}
			stopped := f.list(t, paused.Cursor)
			if !stopped.RunbooksChanged || runbookIDs(stopped.Runbooks) != "chat-task,daily-news" || stopped.Runbooks[1].Status != conversation.ConversationRunbookStopped {
				t.Fatalf("stopped runbook = %#v", stopped.Runbooks)
			}
			if after := f.list(t, stopped.Cursor); after.RunbooksChanged || len(after.Runbooks) != 0 {
				t.Fatalf("stopped runbook reported again: %#v", after.Runbooks)
			}
		})
	}
}

func TestLinkRunbooks(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	service := conversation.NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "links"}
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Chat", IdempotencyKey: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := service.LinkRunbooks(ctx, scope, conversation.ID, []string{"a", "b", "a"})
	if err != nil || strings.Join(linked.RunbookActivationIDs, ",") != "a,b" || linked.Revision != conversation.Revision+1 {
		t.Fatalf("linked = %#v, %v", linked, err)
	}
	again, err := service.LinkRunbooks(ctx, scope, conversation.ID, []string{"b"})
	if err != nil || again.Revision != linked.Revision {
		t.Fatalf("relinking changed the conversation: %#v, %v", again, err)
	}
	ids := make([]string, MaximumConversationRunbooks)
	for index := range ids {
		ids[index] = fmt.Sprintf("r%d", index)
	}
	full, err := service.LinkRunbooks(ctx, scope, conversation.ID, ids)
	if err != nil || len(full.RunbookActivationIDs) != MaximumConversationRunbooks || full.RunbookActivationIDs[0] != "r0" ||
		full.RunbookActivationIDs[MaximumConversationRunbooks-1] != ids[len(ids)-1] {
		t.Fatalf("oldest links did not drop first: %v, %v", full.RunbookActivationIDs, err)
	}
	if _, err := service.LinkRunbooks(ctx, scope, conversation.ID, []string{strings.Repeat("x", 129)}); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := service.LinkRunbooks(ctx, scope, "missing", []string{"a"}); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("missing conversation: %v", err)
	}
	stored, _ := service.GetConversation(ctx, scope, conversation.ID)
	stored.RunbookActivationIDs = append(stored.RunbookActivationIDs, "r0")
	if err := stored.Validate(); err == nil {
		t.Fatal("duplicate link validated")
	}
}

func TestConversationRunbooksIncludeEventRoutines(t *testing.T) {
	for _, testCase := range interactionChangeStores() {
		t.Run(testCase.name, func(t *testing.T) {
			f := newInteractionChangeFixture(t, testCase.store(t))
			ctx, store, scope := f.ctx, f.store, f.scope
			owner := f.conversation.Owner
			now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			f.clock = func() time.Time { return now }
			objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Order notes", Goal: "Summarize each new order", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			routine := storetest.RoutineEventActivation(scope, "shop-orders", now.Add(-2*time.Hour))
			routine.Owner, routine.ObjectiveID, routine.AssignedAgentID = owner, objective.ID, owner.ID
			routine.Task.ConversationID, routine.Task.SourceMessageID = f.conversation.ID, f.trigger.ID
			if err := store.CreateRunbookActivation(ctx, routine); err != nil {
				t.Fatal(err)
			}
			// An event routine from another chat is not shown here.
			other := storetest.RoutineEventActivation(scope, "elsewhere", now.Add(-2*time.Hour))
			other.Owner, other.ObjectiveID, other.AssignedAgentID = owner, objective.ID, owner.ID
			other.Task.ConversationID = "other-chat"
			if err := store.CreateRunbookActivation(ctx, other); err != nil {
				t.Fatal(err)
			}

			initial := f.list(t, "")
			if !initial.RunbooksChanged || runbookIDs(initial.Runbooks) != "shop-orders" {
				t.Fatalf("initial runbooks = %s changed=%v", runbookIDs(initial.Runbooks), initial.RunbooksChanged)
			}
			got := initial.Runbooks[0]
			wantWhen := DescribeRoutineEventTrigger(routine.Task.Event)
			if got.Title != "Order notes" || got.Status != conversation.ConversationRunbookActive || got.Schedule != nil || got.Trigger == nil ||
				got.Trigger.Kind != runbook.TriggerEvent || got.Trigger.EventKind != RoutineEventWebhook || got.Trigger.When == "" ||
				got.Trigger.When != wantWhen || got.Trigger.SourceLabel != "your shop" || got.Trigger.MaxPerHour != 2 ||
				got.LastFiredAt != nil || got.FiredLast24h != 0 || got.Pending != 0 || got.NextRunAt != nil || got.LastRun != nil {
				t.Fatalf("new event routine = %#v trigger=%#v", got, got.Trigger)
			}

			// Two batches start event Runs inside the hour; one more event waits.
			events := store.(RoutineEventStore)
			var lastRun *AgentRun
			for index, ago := range []time.Duration{40 * time.Minute, 10 * time.Minute} {
				at := now.Add(-ago)
				event := storetest.RoutineEventFor(scope, routine, fmt.Sprintf("order-%d", index), at)
				if _, err := events.InsertRoutineEvent(ctx, &RoutineEventRecord{Scope: scope, ActivationID: routine.ID, Event: event, State: RoutineEventPending, ReceivedAt: at}); err != nil {
					t.Fatal(err)
				}
				batch, err := events.ClaimRoutineEventBatch(ctx, scope, routine.ID, RoutineEventMaxBatchEvents, at)
				if err != nil || batch == nil {
					t.Fatalf("claim = %#v, %v", batch, err)
				}
				lastRun, err = NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
					Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, Goal: routine.Task.Goal, Source: RunSourceEvent,
					Context: map[string]interface{}{"runbookActivationId": routine.ID}, IdempotencyKey: batch.ID,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := events.MarkRoutineEventBatchStarted(ctx, scope, batch.ID, lastRun.ID, at); err != nil {
					t.Fatal(err)
				}
			}
			waiting := storetest.RoutineEventFor(scope, routine, "order-waiting", now.Add(-time.Minute))
			if _, err := events.InsertRoutineEvent(ctx, &RoutineEventRecord{Scope: scope, ActivationID: routine.ID, Event: waiting, State: RoutineEventPending, ReceivedAt: now.Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}

			fired := f.list(t, initial.Cursor)
			if !fired.RunbooksChanged || len(fired.Runbooks) != 1 {
				t.Fatalf("fired runbooks = %#v changed=%v", fired.Runbooks, fired.RunbooksChanged)
			}
			got = fired.Runbooks[0]
			wantNext := now.Add(-40 * time.Minute).Add(time.Hour)
			if got.LastFiredAt == nil || !got.LastFiredAt.Equal(now.Add(-10*time.Minute)) || got.FiredLast24h != 2 || got.FiredLastHour != 2 ||
				got.Pending != 1 || got.NextRunAt == nil || !got.NextRunAt.Equal(wantNext) || got.LastRun == nil || got.LastRun.ID != lastRun.ID {
				t.Fatalf("fired event routine = %#v", got)
			}
			if quiet := f.list(t, fired.Cursor); quiet.RunbooksChanged {
				t.Fatalf("unchanged event routine reported again: %#v", quiet.Runbooks)
			}

			// A system pause shows why, and the waiting run time goes away.
			activations := runtimeroutine.NewRunbookActivationService(store, models.WithClock(func() time.Time { return now }))
			current, _ := store.GetRunbookActivation(ctx, scope, routine.ID)
			if _, err := activations.Update(ctx, scope, routine.ID, runtimeroutine.UpdateRunbookActivationRequest{
				ExpectedRevision: current.Revision, Status: RunbookActivationPaused, PausedReason: RoutinePausedNeedsSignIn,
			}); err != nil {
				t.Fatal(err)
			}
			paused := f.list(t, fired.Cursor)
			if !paused.RunbooksChanged || len(paused.Runbooks) != 1 {
				t.Fatalf("paused runbooks = %#v", paused.Runbooks)
			}
			if got := paused.Runbooks[0]; got.Status != conversation.ConversationRunbookPaused || got.PausedReason != RoutinePausedNeedsSignIn || got.NextRunAt != nil {
				t.Fatalf("paused event routine = %#v", got)
			}
		})
	}
}
