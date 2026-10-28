package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func runbookIDs(runbooks []*ConversationRunbook) string {
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
			conversations := NewConversationService(store.(ConversationStore))
			objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: owner, Title: "Morning news", Goal: "Summarise the news every morning", Status: ObjectiveStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
			activations := NewRunbookActivationService(store)
			activations.now = func() time.Time { return now }
			daily, err := activations.Create(ctx, CreateRunbookActivationRequest{
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
				Task:    &ScheduledAgentTask{Goal: "Send the weekly plan", ConversationID: f.conversation.ID, SourceMessageID: f.trigger.ID},
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
			if got := initial.Runbooks[0]; got.Title != "Morning news" || got.Status != ConversationRunbookActive || got.Schedule == nil || got.Schedule.Cron != "0 0 8 * * 1" {
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
			if got := linked.Runbooks[1]; got.ObjectiveID != objective.ID || got.Schedule.Timezone != "Europe/London" || got.NextRunAt != nil || got.LastRun != nil {
				t.Fatalf("daily runbook = %#v", got)
			}

			// The scheduler sets the next run and then starts it.
			scheduler := NewRunbookScheduler(store)
			scheduler.now = func() time.Time { return now }
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
			if _, err := activations.Update(ctx, scope, daily.ID, UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: RunbookActivationPaused}); err != nil {
				t.Fatal(err)
			}
			paused := f.list(t, ran.Cursor)
			if !paused.RunbooksChanged || paused.Runbooks[1].Status != ConversationRunbookPaused || paused.Runbooks[1].NextRunAt != nil {
				t.Fatalf("paused runbook = %#v", paused.Runbooks)
			}
			current, _ = store.GetRunbookActivation(ctx, scope, daily.ID)
			if _, err := activations.Update(ctx, scope, daily.ID, UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: RunbookActivationRetired}); err != nil {
				t.Fatal(err)
			}
			stopped := f.list(t, paused.Cursor)
			if !stopped.RunbooksChanged || runbookIDs(stopped.Runbooks) != "chat-task,daily-news" || stopped.Runbooks[1].Status != ConversationRunbookStopped {
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
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "links"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
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

func TestAnswerVisualLinksItsRoutines(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "routine-links"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}, Title: "News", IdempotencyKey: "routine-links",
	})
	if err != nil {
		t.Fatal(err)
	}
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DeploymentID: "agent-42", DefinitionID: "agent-definition", DefinitionVersion: "1",
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Your routine is set."},
					Visual: &presentation.Visual{Title: "Morning news", Components: []presentation.Component{
						{ID: "r", Type: "routine", Summary: "Your morning news routine.", Binding: &presentation.Binding{Kind: presentation.BindingRunbook, ID: "daily-news"},
							Routine: &presentation.Routine{Items: []presentation.RoutineItem{{Label: "Today", Text: "Rates held."}}}},
					}}}, nil
			}),
		}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns, AttachmentContent: &answerVisualContent{}})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Every morning, the news", "routine-links-1")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run}); err != nil {
		t.Fatal(err)
	}
	stored, err := service.GetConversation(ctx, scope, conversation.ID)
	if err != nil || strings.Join(stored.RunbookActivationIDs, ",") != "daily-news" {
		t.Fatalf("linked routines = %v, %v", stored.RunbookActivationIDs, err)
	}
}
