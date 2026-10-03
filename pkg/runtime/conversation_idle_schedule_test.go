package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type idleScheduleCountingStore struct {
	ConversationStore
	messages, rounds, cursors int
	roundError, cursorError   error
	putCursorError            error
	onCursor                  func()
}

func (s *idleScheduleCountingStore) ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	s.messages++
	return s.ConversationStore.ListChannelMessages(ctx, filter)
}

func (s *idleScheduleCountingStore) ListParticipationRounds(ctx context.Context, filter ParticipationRoundFilter) ([]*ParticipationRoundResult, error) {
	s.rounds++
	if s.roundError != nil {
		return nil, s.roundError
	}
	return s.ConversationStore.ListParticipationRounds(ctx, filter)
}

func (s *idleScheduleCountingStore) GetConversationCursor(ctx context.Context, scope Scope, id string, participant ConversationParticipant) (*ConversationCursor, error) {
	s.cursors++
	if s.cursorError != nil {
		return nil, s.cursorError
	}
	cursor, err := s.ConversationStore.GetConversationCursor(ctx, scope, id, participant)
	if s.onCursor != nil {
		callback := s.onCursor
		s.onCursor = nil
		callback()
	}
	return cursor, err
}

func (s *idleScheduleCountingStore) PutConversationCursor(ctx context.Context, record ConversationCursorRecord) (*ConversationCursor, bool, error) {
	if s.putCursorError != nil {
		err := s.putCursorError
		s.putCursorError = nil
		return nil, false, err
	}
	return s.ConversationStore.PutConversationCursor(ctx, record)
}

func TestConversationIdleSchedulingSkipsCaughtUpPayloadReads(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, kernel KernelStore) {
		for _, empty := range []bool{true, false} {
			t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
				store := kernel.(ConversationStore)
				conversation := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "caught-up"}, fmt.Sprintf("channel-%t", empty))
				if !empty {
					channelMessageBatchCommit(t, store, &conversation, &ChannelMessage{ID: "caught-up-message"})
					idleScheduleAdvanceCursor(t, store, conversation)
				}
				counter := &idleScheduleCountingStore{ConversationStore: store, roundError: errors.New("caught-up history must not be loaded")}
				scheduler := idleScheduleScheduler(t, counter, kernel)
				result := &ConversationRunReconcileResult{}
				if err := scheduler.reconcileConversation(t.Context(), conversation, result); err != nil {
					t.Fatal(err)
				}
				if result.Messages != 0 || counter.messages != 0 || counter.rounds != 0 || counter.cursors != 1 {
					t.Fatalf("caught-up pass read payloads: result=%#v messages=%d rounds=%d cursors=%d", result, counter.messages, counter.rounds, counter.cursors)
				}
			})
		}
	})
}

func TestConversationIdleSchedulingPreservesUnreadPagesAndReplay(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(ConversationStore)
		conversation := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "unread-pages"}, "channel")
		for index := range 5 {
			channelMessageBatchCommit(t, store, &conversation, &ChannelMessage{ID: fmt.Sprintf("unread-%d", index)})
		}
		counter := &idleScheduleCountingStore{ConversationStore: store}
		scheduler := idleScheduleScheduler(t, counter, kernel)
		scheduler.config.MessagePageSize = 2
		result, err := scheduler.ReconcileScope(t.Context(), conversation.Scope)
		if err != nil || result.Messages != 5 || result.Scheduled != 5 || counter.rounds != 1 {
			t.Fatalf("unread reconciliation=%#v rounds=%d error=%v", result, counter.rounds, err)
		}
		cursor, err := store.GetConversationCursor(t.Context(), conversation.Scope, conversation.ID, idleScheduleParticipant())
		if err != nil || cursor == nil || cursor.ReadSequence != 5 {
			t.Fatalf("durable cursor=%#v error=%v", cursor, err)
		}
		counter.messages, counter.rounds = 0, 0
		// A new scheduler has no local state to carry into this replay.
		again, err := idleScheduleScheduler(t, counter, kernel).ReconcileScope(t.Context(), conversation.Scope)
		if err != nil || again.Messages != 0 || counter.messages != 0 || counter.rounds != 0 {
			t.Fatalf("restarted caught-up pass=%#v messages=%d rounds=%d error=%v", again, counter.messages, counter.rounds, err)
		}
		runs, err := kernel.ListAgentRuns(t.Context(), AgentRunFilter{Scope: conversation.Scope, Kind: RunKindConversation})
		if err != nil || len(runs) != 5 {
			t.Fatalf("replay duplicated work: runs=%d error=%v", len(runs), err)
		}
	})
}

func TestConversationIdleSchedulingDoesNotTreatDeliveryAsProcessed(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(ConversationStore)
		conversation := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "delivered-unread"}, "channel")
		channelMessageBatchCommit(t, store, &conversation, &ChannelMessage{ID: "delivered"})
		if _, _, err := NewConversationService(store).AdvanceCursor(t.Context(), AdvanceConversationCursorRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, Participant: idleScheduleParticipant(),
			DeliveredSequence: 1, ReadSequence: 0,
		}); err != nil {
			t.Fatal(err)
		}
		counter := &idleScheduleCountingStore{ConversationStore: store}
		result, err := idleScheduleScheduler(t, counter, kernel).ReconcileScope(t.Context(), conversation.Scope)
		if err != nil || result.Scheduled != 1 || result.Messages != 1 || counter.rounds != 1 {
			t.Fatalf("delivery receipt suppressed unread work: result=%#v rounds=%d error=%v", result, counter.rounds, err)
		}
	})
}

func TestConversationIdleSchedulingRetainsCursorOnCoordinationOrCommitFailure(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, kernel KernelStore) {
		for _, failure := range []string{"cursor_read", "round_read", "cursor_commit"} {
			t.Run(failure, func(t *testing.T) {
				store := kernel.(ConversationStore)
				conversation := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: failure}, "channel")
				channelMessageBatchCommit(t, store, &conversation, &ChannelMessage{ID: "unread"})
				unavailable := errors.New("temporary " + failure + " failure")
				counter := &idleScheduleCountingStore{ConversationStore: store}
				switch failure {
				case "cursor_read":
					counter.cursorError = unavailable
				case "round_read":
					counter.roundError = unavailable
				case "cursor_commit":
					counter.putCursorError = unavailable
				}
				if _, err := idleScheduleScheduler(t, counter, kernel).ReconcileScope(t.Context(), conversation.Scope); !errors.Is(err, unavailable) {
					t.Fatalf("recovery read or commit failure=%v", err)
				}
				cursor, err := store.GetConversationCursor(t.Context(), conversation.Scope, conversation.ID, idleScheduleParticipant())
				if err != nil || cursor != nil {
					t.Fatalf("failed page advanced cursor=%#v error=%v", cursor, err)
				}
				counter.cursorError, counter.roundError = nil, nil
				result, err := idleScheduleScheduler(t, counter, kernel).ReconcileScope(t.Context(), conversation.Scope)
				if err != nil || result.Messages != 1 || result.Scheduled+result.Replayed != 1 {
					t.Fatalf("restart failed to recover exact unread message=%#v error=%v", result, err)
				}
				runs, err := kernel.ListAgentRuns(t.Context(), AgentRunFilter{Scope: conversation.Scope, Kind: RunKindConversation})
				if err != nil || len(runs) != 1 || runs[0].Context[conversationRunContextTriggerID] != "unread" {
					t.Fatalf("recovery lost or duplicated work: runs=%#v error=%v", runs, err)
				}
			})
		}
	})
}

func TestConversationIdleSchedulingRecoversConcurrentPostAfterCaughtUpSnapshot(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(ConversationStore)
		conversation := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "concurrent-post"}, "channel")
		channelMessageBatchCommit(t, store, &conversation, &ChannelMessage{ID: "first"})
		idleScheduleAdvanceCursor(t, store, conversation)
		staleSnapshot := cloneConversation(conversation)
		counter := &idleScheduleCountingStore{ConversationStore: store, onCursor: func() {
			channelMessageBatchCommit(t, store, &conversation, &ChannelMessage{ID: "later"})
		}}
		if err := idleScheduleScheduler(t, counter, kernel).reconcileConversation(t.Context(), staleSnapshot, &ConversationRunReconcileResult{}); err != nil {
			t.Fatal(err)
		}
		if counter.messages != 0 || counter.rounds != 0 {
			t.Fatal("caught-up snapshot unexpectedly hydrated history")
		}
		result, err := idleScheduleScheduler(t, counter, kernel).ReconcileScope(t.Context(), conversation.Scope)
		if err != nil || result.Messages != 1 || result.Scheduled != 1 {
			t.Fatalf("concurrent post was lost by recovery=%#v error=%v", result, err)
		}
		runs, err := kernel.ListAgentRuns(t.Context(), AgentRunFilter{Scope: conversation.Scope, Kind: RunKindConversation})
		if err != nil || len(runs) != 1 || runs[0].Context[conversationRunContextTriggerID] != "later" {
			t.Fatalf("wrong concurrent message scheduled: runs=%#v error=%v", runs, err)
		}
	})
}

func TestConversationIdleSchedulingHonorsCommittedRoundAndScopedVisibility(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(ConversationStore)
		first := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "first"}, "same-channel")
		channelMessageBatchCommit(t, store, &first, &ChannelMessage{ID: "first-message"})
		idleScheduleAdvanceCursor(t, store, first)
		second := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "second"}, "same-channel")
		channelMessageBatchCommit(t, store, &second, &ChannelMessage{ID: "coordinated"})
		service := NewConversationService(store)
		if _, err := service.CoordinateParticipation(t.Context(), CoordinateParticipationRequest{
			Scope: second.Scope, ConversationID: second.ID, ExpectedRevision: second.Revision,
			TriggerMessageID: "coordinated", Policy: DefaultConversationArbitrationPolicy(), IdempotencyKey: "silent-round",
			Proposals: []ParticipationProposal{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "silent-agent"}, WantsToSpeak: false}},
		}); err != nil {
			t.Fatal(err)
		}
		second, _ = store.GetConversation(t.Context(), second.Scope, second.ID)
		channelMessageBatchCommit(t, store, &second, &ChannelMessage{ID: "private-unread", Audience: ConversationAudience{Kind: ConversationAudienceRoles, Roles: []string{"reviewer"}}})
		counter := &idleScheduleCountingStore{ConversationStore: store}
		result, err := idleScheduleScheduler(t, counter, kernel).ReconcileScope(t.Context(), second.Scope)
		if err != nil || result.Messages != 2 || result.Skipped != 1 || result.Scheduled != 1 || counter.rounds != 1 {
			t.Fatalf("scoped coordination=%#v rounds=%d error=%v", result, counter.rounds, err)
		}
		runs, err := kernel.ListAgentRuns(t.Context(), AgentRunFilter{Scope: second.Scope, Kind: RunKindConversation})
		if err != nil || len(runs) != 1 || runs[0].Context[conversationRunContextTriggerID] != "private-unread" {
			t.Fatalf("scoped cursor skipped unprocessed input: runs=%#v error=%v", runs, err)
		}
		viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "reader"}}
		if _, err := service.GetVisibleChannelMessage(t.Context(), second.Scope, second.ID, "private-unread", viewer); !errors.Is(err, ErrChannelMessageNotFound) {
			t.Fatalf("private unread message leaked=%v", err)
		}
		viewer.Roles = []string{"reviewer"}
		if _, err := service.GetVisibleChannelMessage(t.Context(), second.Scope, second.ID, "private-unread", viewer); err != nil {
			t.Fatal(err)
		}
	})
}

func idleScheduleParticipant() ConversationParticipant {
	return ConversationParticipant{Type: ConversationParticipantService, ID: conversationRunSchedulerParticipant}
}

func idleScheduleAdvanceCursor(t testing.TB, store ConversationStore, conversation *Conversation) {
	t.Helper()
	if _, _, err := NewConversationService(store).AdvanceCursor(context.Background(), AdvanceConversationCursorRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, Participant: idleScheduleParticipant(),
		DeliveredSequence: conversation.LastSequence, ReadSequence: conversation.LastSequence,
	}); err != nil {
		t.Fatal(err)
	}
}

func idleScheduleScheduler(t testing.TB, conversations ConversationStore, runs RunCommandStore) *ConversationRunScheduler {
	t.Helper()
	scheduler, err := NewConversationRunScheduler(conversations, runs, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}
