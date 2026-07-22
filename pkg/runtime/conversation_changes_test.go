package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestConversationChangesAreCursorStableAndProjectDurableState(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore(50) }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "conversation-changes.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			conversationStore := store.(ConversationStore)
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "changes"}
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			conversations := NewConversationService(conversationStore)
			conversations.now = func() time.Time { return now }
			conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
				Title: "Operations", IdempotencyKey: "operations-channel",
			})
			if err != nil {
				t.Fatal(err)
			}
			question, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
				Content: "What is the release state?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				RequiresResponse: true, IdempotencyKey: "release-question",
			})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewConversationRunScheduler(conversationStore, store, ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, question.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			changes, err := NewConversationChangeService(conversationStore, store)
			if err != nil {
				t.Fatal(err)
			}
			changes.now = func() time.Time { return now }
			initial, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, ActiveAt: now,
			})
			if err != nil || !initial.HasChanges || initial.HasMore || !initial.RunsChanged || !initial.ActivityChanged || !initial.PresenceChanged ||
				len(initial.Messages) != 1 || len(initial.Rounds) != 0 ||
				len(initial.Runs) != 1 || initial.Runs[0].ID != scheduled.Run.ID || initial.Runs[0].Status != AgentRunStatusQueued ||
				len(initial.Activity) != 1 || initial.Activity[0].EventType != "run.created" || initial.Activity[0].Category != "run" ||
				initial.Activity[0].Payload != nil || !initial.Activity[0].DetailAvailable ||
				len(initial.Presence) != 0 || initial.Cursor == "" {
				t.Fatalf("initial changes = %#v, %v", initial, err)
			}
			unchanged, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor, ActiveAt: now,
			})
			if err != nil || unchanged.HasChanges || unchanged.RunsChanged || unchanged.ActivityChanged || unchanged.PresenceChanged ||
				len(unchanged.Messages) != 0 || len(unchanged.Rounds) != 0 ||
				len(unchanged.Runs) != 0 || len(unchanged.Activity) != 0 || len(unchanged.Presence) != 0 || unchanged.Cursor != initial.Cursor {
				t.Fatalf("unchanged projection = %#v, %v", unchanged, err)
			}

			running, _, err := NewRunActivityService(store, store).TransitionRun(ctx, scope, scheduled.Run.ID, RunTransitionRequest{
				ExpectedRevision: scheduled.Run.Revision, Status: AgentRunStatusRunning,
				Actor: ActivityActor{Type: "worker", ID: "conversation-worker"}, Summary: "Review started",
			})
			if err != nil {
				t.Fatal(err)
			}
			presence, err := conversations.SetPresence(ctx, SetConversationPresenceRequest{
				Scope: scope, ConversationID: conversation.ID,
				Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "accountant"},
				State:       ConversationPresenceWorking, Summary: "Reviewing evidence", RunID: running.ID, TTL: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			working, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor, ActiveAt: now,
			})
			if err != nil || !working.HasChanges || !working.RunsChanged || !working.ActivityChanged || !working.PresenceChanged ||
				len(working.Runs) != 1 || working.Runs[0].Status != AgentRunStatusRunning ||
				len(working.Activity) != 2 || working.Activity[0].Summary != "Review started" ||
				len(working.Presence) != 1 || working.Presence[0].LeaseID != presence.LeaseID {
				t.Fatalf("working projection = %#v, %v", working, err)
			}

			current, err := conversations.GetConversation(ctx, scope, conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			round, err := conversations.CoordinateParticipation(ctx, CoordinateParticipationRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
				TriggerMessageID: question.Message.ID, IdempotencyKey: "release-round",
				Proposals: []ParticipationProposal{{
					Participant:  ConversationParticipant{Type: ConversationParticipantAgent, ID: "accountant"},
					WantsToSpeak: true, Intent: MessageIntentAnswer,
					Content:  "The reconciled ledger evidence is attached and the control passed.",
					Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: question.Message.ID,
					Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, HasEvidence: true, RoleRelevant: true},
				}},
			})
			if err != nil || round.Round.ConversationRevision != 3 || len(round.Messages) != 1 {
				t.Fatalf("round = %#v, %v", round, err)
			}
			afterRound, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: working.Cursor, ActiveAt: now,
			})
			if err != nil || !afterRound.HasChanges || len(afterRound.Messages) != 1 || afterRound.Messages[0].ParticipationRoundID != round.Round.ID ||
				len(afterRound.Rounds) != 1 || afterRound.Rounds[0].Round.ID != round.Round.ID {
				t.Fatalf("round changes = %#v, %v", afterRound, err)
			}
			expired, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: afterRound.Cursor, ActiveAt: now.Add(time.Minute),
			})
			if err != nil || !expired.HasChanges || !expired.PresenceChanged || len(expired.Presence) != 0 {
				t.Fatalf("expired presence changes = %#v, %v", expired, err)
			}
			if _, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: conversation.ID, Cursor: "not-a-cursor",
			}); err == nil {
				t.Fatal("invalid cursor was accepted")
			}
			if _, err := changes.ListChanges(ctx, ConversationChangeRequest{
				Scope: scope, ConversationID: "another-channel", Cursor: initial.Cursor,
			}); err == nil {
				t.Fatal("cross-conversation cursor was accepted")
			}
		})
	}
}

func TestConversationChangesAdvanceOpaqueCursorAcrossHiddenPage(t *testing.T) {
	store := NewMemoryStore(100)
	conversations := NewConversationService(store)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "private-stream"}
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Private stream", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	sender := ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}
	target := ConversationParticipant{Type: ConversationParticipantAgent, ID: "target"}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "viewer"}}
	hidden, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: sender, Intent: MessageIntentUpdate, Content: "target only", Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{target}}, IdempotencyKey: "hidden"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = conversations.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: hidden.Conversation.Revision, Sender: sender, Intent: MessageIntentUpdate, Content: "channel update", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "public"})
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := NewConversationChangeService(store, store)
	first, err := changes.ListChanges(ctx, ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Limit: 1, Viewer: &viewer})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 0 || !first.HasMore || first.Cursor == "" {
		t.Fatalf("first=%#v", first)
	}
	second, err := changes.ListChanges(ctx, ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: first.Cursor, Limit: 1, Viewer: &viewer})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 1 || second.Messages[0].Content != "channel update" {
		t.Fatalf("second=%#v", second)
	}
}

func TestConversationChangeCursorSurvivesSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conversation-change-restart.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "restart"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
		Title: "Restart", IdempotencyKey: "restart-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentUpdate,
		Content: "First durable fact", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := NewConversationChangeService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := changes.ListChanges(ctx, ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(initial.Messages) != 1 || initial.Messages[0].ID != first.Message.ID {
		t.Fatalf("initial changes = %#v, %v", initial, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedConversations := NewConversationService(reopened)
	current, err := restartedConversations.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restartedConversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentUpdate,
		Content: "Second durable fact", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedChanges, err := NewConversationChangeService(reopened, reopened)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := restartedChanges.ListChanges(ctx, ConversationChangeRequest{
		Scope: scope, ConversationID: conversation.ID, Cursor: initial.Cursor,
	})
	if err != nil || len(resumed.Messages) != 1 || resumed.Messages[0].ID != second.Message.ID {
		t.Fatalf("resumed changes = %#v, %v", resumed, err)
	}
}
