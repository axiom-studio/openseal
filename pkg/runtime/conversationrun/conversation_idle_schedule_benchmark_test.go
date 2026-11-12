package conversationrun

import (
	"context"
	"fmt"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// Count authoritative reads while retaining the canonical SQLiteStore paths.
// These benchmarks measure reconciliation work, not SQL latency or serving CPU.
type idleConversationBenchmarkStore struct {
	*sqlite.Store
	messageReads uint64
	roundReads   uint64
	roundRows    uint64
	cursorReads  uint64
}

func (s *idleConversationBenchmarkStore) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	s.messageReads++
	return s.Store.ListChannelMessages(ctx, filter)
}

func (s *idleConversationBenchmarkStore) ListParticipationRounds(ctx context.Context, filter models.ParticipationRoundFilter) ([]*models.ParticipationRoundResult, error) {
	s.roundReads++
	rounds, err := s.Store.ListParticipationRounds(ctx, filter)
	s.roundRows += uint64(len(rounds))
	return rounds, err
}

func (s *idleConversationBenchmarkStore) GetConversationCursor(ctx context.Context, scope models.Scope, id string, participant models.ConversationParticipant) (*models.ConversationCursor, error) {
	s.cursorReads++
	return s.Store.GetConversationCursor(ctx, scope, id, participant)
}

func (s *idleConversationBenchmarkStore) resetReads() {
	s.messageReads, s.roundReads, s.roundRows, s.cursorReads = 0, 0, 0, 0
}

func (s *idleConversationBenchmarkStore) reportReads(b *testing.B) {
	b.ReportMetric(float64(s.messageReads)/float64(b.N), "message_reads/op")
	b.ReportMetric(float64(s.roundReads)/float64(b.N), "round_reads/op")
	b.ReportMetric(float64(s.roundRows)/float64(b.N), "round_rows/op")
	b.ReportMetric(float64(s.cursorReads)/float64(b.N), "cursor_reads/op")
}

func benchmarkCaughtUpConversations(b *testing.B, count, historicalRounds int) (*idleConversationBenchmarkStore, *ConversationRunScheduler, models.Scope, []*models.Conversation) {
	b.Helper()
	store := &idleConversationBenchmarkStore{Store: kerneltest.NewStore(b)}
	service := runtimeconversation.NewConversationService(store)
	scope := models.Scope{Kind: "tenant", ID: "idle-scheduler-benchmark"}
	ctx := b.Context()
	conversations := make([]*models.Conversation, 0, count)
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("caught-up-%05d", index)
		conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
			ID: id, Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "benchmark-team"},
			Title: id, IdempotencyKey: id,
		})
		if err != nil {
			b.Fatal(err)
		}
		posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
			Scope: scope, ConversationID: id, ExpectedRevision: conversation.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "benchmark-user"},
			Intent: models.MessageIntentQuestion, Content: "Review this request.", RequiresResponse: true,
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "trigger",
		})
		if err != nil {
			b.Fatal(err)
		}
		conversation = posted.Conversation
		for round := 0; round < historicalRounds; round++ {
			committed, err := service.CommitConversationRouting(ctx, runtimeconversation.CommitConversationRoutingRequest{
				Scope: scope, ConversationID: id, ExpectedRevision: conversation.Revision,
				TriggerMessageID: posted.Message.ID, IdempotencyKey: fmt.Sprintf("round-%05d", round),
			})
			if err != nil {
				b.Fatal(err)
			}
			conversation = committed.Conversation
		}
		if _, _, err := service.AdvanceCursor(ctx, runtimeconversation.AdvanceConversationCursorRequest{
			Scope: scope, ConversationID: id,
			Participant:       models.ConversationParticipant{Type: models.ConversationParticipantService, ID: models.ConversationRunSchedulerParticipant},
			DeliveredSequence: conversation.LastSequence, ReadSequence: conversation.LastSequence,
		}); err != nil {
			b.Fatal(err)
		}
		conversations = append(conversations, conversation)
	}
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{
		ConversationPageSize: 100, MessagePageSize: 100,
	})
	if err != nil {
		b.Fatal(err)
	}
	store.resetReads()
	return store, scheduler, scope, conversations
}

// Each channel contains a committed trigger and round with a persisted scheduler
// cursor at LastSequence. Every timed call executes the public recovery pass.
func BenchmarkConversationSchedulerCaughtUpScope(b *testing.B) {
	for _, count := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("conversations_%d", count), func(b *testing.B) {
			store, scheduler, scope, _ := benchmarkCaughtUpConversations(b, count, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, err := scheduler.ReconcileScope(b.Context(), scope)
				if err != nil || result == nil || result.Conversations != count || result.Messages != 0 || result.Scheduled != 0 || result.Replayed != 0 {
					b.Fatalf("caught-up scope reconciliation = %#v, %v", result, err)
				}
			}
			b.StopTimer()
			store.reportReads(b)
		})
	}
}

// Isolate the actual per-channel scheduler path from scope listing and unrelated
// recovery passes. Setup creates real history; the benchmark fabricates no
// cached trigger index and makes no alternative reconciliation implementation.
func BenchmarkConversationSchedulerCaughtUpHistory(b *testing.B) {
	for _, rounds := range []int{0, 100, 1000} {
		b.Run(fmt.Sprintf("rounds_%d", rounds), func(b *testing.B) {
			store, scheduler, _, conversations := benchmarkCaughtUpConversations(b, 1, rounds)
			conversation := conversations[0]
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var result ConversationRunReconcileResult
				if err := scheduler.reconcileConversation(b.Context(), conversation, &result); err != nil || result.Messages != 0 || result.Scheduled != 0 || result.Replayed != 0 {
					b.Fatalf("caught-up history reconciliation = %#v, %v", result, err)
				}
			}
			b.StopTimer()
			store.reportReads(b)
		})
	}
}
