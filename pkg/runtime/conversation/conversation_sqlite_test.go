package conversation_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestSQLiteConversationServiceSurvivesRestartAndConcurrentReplicas(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "natural-channels.db")
	primary, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	primaryService := runtimeconversation.NewConversationService(primary, models.WithClock(func() time.Time { return fixed }))
	replicaService := runtimeconversation.NewConversationService(replica, models.WithClock(func() time.Time { return fixed }))
	scope := models.Scope{Kind: "tenant", ID: "one"}
	conversationtest.SeedSpace(t, primary, scope)
	conversation, _, err := primaryService.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "product"}, Title: "Release", IdempotencyKey: "release-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	question, err := primaryService.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "operator"}, Intent: models.MessageIntentQuestion,
		Content: "Is it ready?", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "question-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "developer"}
	cursor, _, err := primaryService.AdvanceCursor(ctx, runtimeconversation.AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent,
		DeliveredSequence: question.Message.Sequence, ReadSequence: question.Message.Sequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	presence, err := primaryService.SetPresence(ctx, runtimeconversation.SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: models.ConversationPresenceWorking,
		Summary: "Checking release", RunID: "release-run", TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	thinkingParticipant := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "reviewer"}
	thinkingPresence, err := primaryService.SetPresence(ctx, runtimeconversation.SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: thinkingParticipant,
		State: models.ConversationPresenceThinking, Summary: "Evaluating evidence", TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	typingParticipant := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "release-manager"}
	typingPresence, err := primaryService.SetPresence(ctx, runtimeconversation.SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: typingParticipant,
		State: models.ConversationPresenceTyping, Summary: "Writing the decision", TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	roundRequest := runtimeconversation.CommitConversationRoutingRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: question.Conversation.Revision,
		TriggerMessageID: question.Message.ID, IdempotencyKey: "round-v1",
		Responders: []models.ConversationResponder{{Participant: agent, Addressed: models.ConversationAddressedRouted, Reason: "owns the release"}},
	}
	services := []*runtimeconversation.ConversationService{primaryService, replicaService}
	var committed atomic.Int32
	var failed atomic.Int32
	failureErrors := make(chan error, 12)
	var wait sync.WaitGroup
	for index := 0; index < 12; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := services[index%len(services)].CommitConversationRouting(ctx, roundRequest)
			if err != nil {
				failed.Add(1)
				failureErrors <- err
				return
			}
			if !result.Replayed {
				committed.Add(1)
			}
		}()
	}
	wait.Wait()
	close(failureErrors)
	if failed.Load() != 0 || committed.Load() != 1 {
		errorsSeen := make([]error, 0, failed.Load())
		for err := range failureErrors {
			errorsSeen = append(errorsSeen, err)
		}
		t.Fatalf("failed = %d, committed = %d, errors = %v", failed.Load(), committed.Load(), errorsSeen)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedServiceClock := func() time.Time { return fixed }
	restartedService := runtimeconversation.NewConversationService(restarted, models.WithClock(func() time.Time { return restartedServiceClock() }))
	restored, err := restartedService.GetConversation(ctx, scope, conversation.ID)
	if err != nil || restored.Revision != 3 || restored.LastSequence != 1 {
		t.Fatalf("restored conversation = %#v, err = %v", restored, err)
	}
	messages, err := restartedService.ListChannelMessages(ctx, models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 1 {
		t.Fatalf("restored messages = %#v, err = %v", messages, err)
	}
	restoredCursor, err := restarted.GetConversationCursor(ctx, scope, conversation.ID, agent)
	if err != nil || restoredCursor.Revision != cursor.Revision || restoredCursor.ReadSequence != 1 {
		t.Fatalf("restored cursor = %#v, err = %v", restoredCursor, err)
	}
	restoredPresence, err := restarted.GetConversationPresence(ctx, scope, conversation.ID, agent)
	if err != nil || restoredPresence.LeaseID != presence.LeaseID || restoredPresence.State != models.ConversationPresenceWorking || restoredPresence.RunID != "release-run" {
		t.Fatalf("restored presence = %#v, err = %v", restoredPresence, err)
	}
	restoredThinking, err := restarted.GetConversationPresence(ctx, scope, conversation.ID, thinkingParticipant)
	if err != nil || restoredThinking.LeaseID != thinkingPresence.LeaseID || restoredThinking.State != models.ConversationPresenceThinking || restoredThinking.RunID != "" {
		t.Fatalf("restored thinking presence = %#v, err = %v", restoredThinking, err)
	}
	restoredTyping, err := restarted.GetConversationPresence(ctx, scope, conversation.ID, typingParticipant)
	if err != nil || restoredTyping.LeaseID != typingPresence.LeaseID || restoredTyping.State != models.ConversationPresenceTyping || restoredTyping.RunID != "" {
		t.Fatalf("restored typing presence = %#v, err = %v", restoredTyping, err)
	}
	roundReplay, err := restartedService.CommitConversationRouting(ctx, roundRequest)
	if err != nil || !roundReplay.Replayed || len(roundReplay.Messages) != 0 {
		t.Fatalf("restored round replay = %#v, err = %v", roundReplay, err)
	}
	loadedRound, err := restartedService.GetParticipationRound(ctx, scope, conversation.ID, roundReplay.Round.ID)
	if err != nil || len(loadedRound.Round.Responders) != 1 || loadedRound.Round.Responders[0].Participant != agent ||
		loadedRound.Round.Responders[0].Reason != "owns the release" {
		t.Fatalf("loaded round = %#v, err = %v", loadedRound, err)
	}
	rounds, err := restartedService.ListParticipationRounds(ctx, models.ParticipationRoundFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(rounds) != 1 || rounds[0].Round.ID != roundReplay.Round.ID {
		t.Fatalf("rounds = %#v, err = %v", rounds, err)
	}
	if foreign, err := restartedService.GetConversation(ctx, models.Scope{Kind: "tenant", ID: "other"}, conversation.ID); err == nil || foreign != nil {
		t.Fatalf("foreign conversation = %#v, err = %v", foreign, err)
	}
	restartedServiceClock = func() time.Time { return fixed.Add(31 * time.Second) }
	active, err := restartedService.ListPresence(ctx, scope, conversation.ID)
	if err != nil || len(active) != 0 {
		t.Fatalf("expired presence = %#v, err = %v", active, err)
	}
}
