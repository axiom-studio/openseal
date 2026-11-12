package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestConversationChangesAreCursorStableAndProjectDurableState(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) models.KernelStore
	}{
		{name: "sqlite", store: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "conversation-changes.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			conversationStore := store.(models.ConversationStore)
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "changes"}
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			conversations := runtimeconversation.NewConversationService(conversationStore, models.WithClock(func() time.Time { return now }))
			seedTestSpace(t, store.(models.SpaceStore), scope)
			conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "operations"},
				Title: "Operations", IdempotencyKey: "operations-channel",
			})
			if err != nil {
				t.Fatal(err)
			}
			question, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "operator"}, Intent: models.MessageIntentQuestion,
				Content: "What is the release state?", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
				RequiresResponse: true, IdempotencyKey: "release-question",
			})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := conversationrun.NewConversationRunScheduler(conversationStore, store, conversationrun.ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, question.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			changes, err := runtimeconversation.NewConversationChangeService(conversationStore, store, models.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			initial, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, ActiveAt: now,
			})
			if err != nil || !initial.HasChanges || initial.HasMore || !initial.RunsChanged || !initial.ActivityChanged || !initial.PresenceChanged ||
				len(initial.Messages) != 1 || len(initial.Rounds) != 0 ||
				len(initial.Runs) != 1 || initial.Runs[0].ID != scheduled.Run.ID || initial.Runs[0].Status != models.AgentRunStatusQueued ||
				len(initial.Activity) != 1 || initial.Activity[0].EventType != "run.created" || initial.Activity[0].Category != "run" ||
				initial.Activity[0].Payload != nil || !initial.Activity[0].DetailAvailable ||
				len(initial.Presence) != 0 || initial.Cursor == "" {
				t.Fatalf("initial changes = %#v, %v", initial, err)
			}
			unchanged, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor, ActiveAt: now,
			})
			if err != nil || unchanged.HasChanges || unchanged.RunsChanged || unchanged.ActivityChanged || unchanged.PresenceChanged ||
				len(unchanged.Messages) != 0 || len(unchanged.Rounds) != 0 ||
				len(unchanged.Runs) != 0 || len(unchanged.Activity) != 0 || len(unchanged.Presence) != 0 || unchanged.Cursor != initial.Cursor {
				t.Fatalf("unchanged projection = %#v, %v", unchanged, err)
			}

			running, _, err := activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, scheduled.Run.ID, activity.RunTransitionRequest{
				ExpectedRevision: scheduled.Run.Revision, Status: models.AgentRunStatusRunning,
				Actor: models.ActivityActor{Type: "worker", ID: "conversation-worker"}, Summary: "Review started",
				ActivityUsageDelta: &models.BudgetUsage{Turns: 1, InputTokens: 80, OutputTokens: 20, CostMicros: 50_000},
			})
			if err != nil {
				t.Fatal(err)
			}
			presence, err := conversations.SetPresence(ctx, runtimeconversation.SetConversationPresenceRequest{
				Scope: scope, ConversationID: conversation.ID,
				Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "accountant"},
				State:       models.ConversationPresenceWorking, Summary: "Reviewing evidence", RunID: running.ID, TTL: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			working, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor, ActiveAt: now,
			})
			if err != nil || !working.HasChanges || !working.RunsChanged || !working.ActivityChanged || !working.PresenceChanged ||
				len(working.Runs) != 1 || working.Runs[0].Status != models.AgentRunStatusRunning ||
				len(working.Activity) != 2 || working.Activity[0].Summary != "Review started" ||
				working.Activity[0].UsageDelta == nil || working.Activity[0].UsageDelta.InputTokens != 80 ||
				working.Activity[0].UsageDelta.OutputTokens != 20 || working.Activity[0].UsageDelta.CostMicros != 50_000 ||
				len(working.Presence) != 1 || working.Presence[0].LeaseID != presence.LeaseID {
				t.Fatalf("working projection = %#v, %v", working, err)
			}

			current, err := conversations.GetConversation(ctx, scope, conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			round, err := conversations.CommitConversationRouting(ctx, runtimeconversation.CommitConversationRoutingRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
				TriggerMessageID: question.Message.ID, IdempotencyKey: "release-round",
				Responders: []models.ConversationResponder{{
					Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "accountant"},
					Addressed:   models.ConversationAddressedRouted, Reason: "owns the ledger",
				}},
			})
			if err != nil || round.Round.ConversationRevision != 3 || len(round.Messages) != 0 {
				t.Fatalf("round = %#v, %v", round, err)
			}
			afterRound, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: working.Cursor, ActiveAt: now,
			})
			if err != nil || !afterRound.HasChanges || len(afterRound.Rounds) != 1 || afterRound.Rounds[0].Round.ID != round.Round.ID ||
				afterRound.Rounds[0].Round.Responders[0].Reason != "owns the ledger" {
				t.Fatalf("round changes = %#v, %v", afterRound, err)
			}
			expired, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: afterRound.Cursor, ActiveAt: now.Add(time.Minute),
			})
			if err != nil || !expired.HasChanges || !expired.PresenceChanged || len(expired.Presence) != 0 {
				t.Fatalf("expired presence changes = %#v, %v", expired, err)
			}
			if _, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: "not-a-cursor",
			}); err == nil {
				t.Fatal("invalid cursor was accepted")
			}
			if _, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
				Scope: scope, ConversationID: "another-channel", Cursor: initial.Cursor,
			}); err == nil {
				t.Fatal("cross-conversation cursor was accepted")
			}
		})
	}
}

func TestConversationChangesAdvanceOpaqueCursorAcrossHiddenPage(t *testing.T) {
	store := newTestKernelStore(t)
	conversations := runtimeconversation.NewConversationService(store)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "private-stream"}
	seedTestSpace(t, store, scope)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, Title: "Private stream", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	sender := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "sender"}
	target := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "target"}
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "viewer"}}
	hidden, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: sender, Intent: models.MessageIntentUpdate, Content: "target only", Audience: models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{target}}, IdempotencyKey: "hidden"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: hidden.Conversation.Revision, Sender: sender, Intent: models.MessageIntentUpdate, Content: "channel update", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "public"})
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := runtimeconversation.NewConversationChangeService(store, store)
	now := time.Now().UTC()
	for _, run := range []*models.AgentRun{
		{ID: "hidden-root", Kind: models.RunKindConversation, RootRunID: "hidden-root", ConcurrencyKey: conversation.ID, Context: map[string]interface{}{models.ConversationRunTriggerIDContextKey: hidden.Message.ID}},
		{ID: "hidden-child", Kind: models.RunKindAgentWork, RootRunID: "hidden-root", ParentRunID: "hidden-root"},
	} {
		run.Scope, run.Owner = scope, conversation.Owner
		run.Goal, run.Source, run.Status = "Private task contents", models.RunSourceChat, models.AgentRunStatusRunning
		run.Revision, run.CreatedAt, run.UpdatedAt, run.AvailableAt, run.QueueEnteredAt = 1, now, now, now, now
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	first, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Limit: 1, Viewer: &viewer})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 0 || !first.HasMore || first.Cursor == "" {
		t.Fatalf("first=%#v", first)
	}
	if len(first.Runs) != 0 || len(first.Activity) != 0 {
		t.Fatal("private execution leaked to a viewer without message access")
	}
	allowed := models.ConversationViewer{Participant: target}
	private, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Viewer: &allowed})
	if err != nil || len(private.Runs) != 2 {
		t.Fatalf("authorized viewer lost execution: %#v, %v", private, err)
	}
	second, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: first.Cursor, Limit: 1, Viewer: &viewer})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 1 || second.Messages[0].Content != "channel update" {
		t.Fatalf("second=%#v", second)
	}
	if _, _, err := activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, "hidden-child", activity.RunTransitionRequest{
		ExpectedRevision: 1, Status: models.AgentRunStatusFailed,
		Actor: models.ActivityActor{Type: "worker", ID: "private-worker"}, Summary: "Private failure details",
	}); err != nil {
		t.Fatal(err)
	}
	afterHiddenChange, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: second.Cursor, Viewer: &viewer})
	if err != nil || afterHiddenChange.HasChanges || afterHiddenChange.Cursor != second.Cursor || len(afterHiddenChange.Activity) != 0 {
		t.Fatalf("hidden work leaked through cursor or activity: %#v, %v", afterHiddenChange, err)
	}
	private, err = changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Viewer: &allowed})
	if err != nil || len(private.Activity) == 0 {
		t.Fatalf("authorized viewer lost private activity: %#v, %v", private, err)
	}
}

func TestConversationChangeCursorSurvivesSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conversation-change-restart.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: "restart"}
	conversations := runtimeconversation.NewConversationService(store)
	seedTestSpace(t, store, scope)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "operations"},
		Title: "Restart", IdempotencyKey: "restart-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "operator"}, Intent: models.MessageIntentUpdate,
		Content: "First durable fact", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := runtimeconversation.NewConversationChangeService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(initial.Messages) != 1 || initial.Messages[0].ID != first.Message.ID {
		t.Fatalf("initial changes = %#v, %v", initial, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedConversations := runtimeconversation.NewConversationService(reopened)
	current, err := restartedConversations.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restartedConversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "operator"}, Intent: models.MessageIntentUpdate,
		Content: "Second durable fact", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedChanges, err := runtimeconversation.NewConversationChangeService(reopened, reopened)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := restartedChanges.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{
		Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor,
	})
	if err != nil || len(resumed.Messages) != 1 || resumed.Messages[0].ID != second.Message.ID {
		t.Fatalf("resumed changes = %#v, %v", resumed, err)
	}
}
