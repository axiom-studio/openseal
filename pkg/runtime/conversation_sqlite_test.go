package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSQLiteConversationServiceSurvivesRestartAndConcurrentReplicas(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "natural-channels.db")
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	primaryService := NewConversationService(primary)
	primaryService.now = func() time.Time { return fixed }
	replicaService := NewConversationService(replica)
	replicaService.now = func() time.Time { return fixed }
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation, _, err := primaryService.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "product"}, Title: "Release", IdempotencyKey: "release-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	question, err := primaryService.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Is it ready?", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "question-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}
	cursor, _, err := primaryService.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent,
		DeliveredSequence: question.Message.Sequence, ReadSequence: question.Message.Sequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	presence, err := primaryService.SetPresence(ctx, SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: ConversationPresenceWorking,
		Summary: "Checking release", RunID: "release-run", TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	roundRequest := CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: question.Conversation.Revision,
		TriggerMessageID: question.Message.ID, IdempotencyKey: "round-v1",
		Proposals: []ParticipationProposal{{
			Participant: agent, WantsToSpeak: true, Intent: MessageIntentAnswer,
			Content: "It is ready and the smoke-test evidence passed.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: question.Message.ID, ResolvesMessageID: question.Message.ID,
			Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true, HasEvidence: true},
		}},
	}
	services := []*ConversationService{primaryService, replicaService}
	var committed atomic.Int32
	var failed atomic.Int32
	failureErrors := make(chan error, 12)
	var wait sync.WaitGroup
	for index := 0; index < 12; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := services[index%len(services)].CoordinateParticipation(ctx, roundRequest)
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

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedService := NewConversationService(restarted)
	restartedService.now = func() time.Time { return fixed }
	restored, err := restartedService.GetConversation(ctx, scope, conversation.ID)
	if err != nil || restored.Revision != 3 || restored.LastSequence != 2 {
		t.Fatalf("restored conversation = %#v, err = %v", restored, err)
	}
	messages, err := restartedService.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[1].ThreadRootID != question.Message.ID || messages[1].ResolvesMessageID != question.Message.ID {
		t.Fatalf("restored messages = %#v, err = %v", messages, err)
	}
	restoredCursor, err := restarted.GetConversationCursor(ctx, scope, conversation.ID, agent)
	if err != nil || restoredCursor.Revision != cursor.Revision || restoredCursor.ReadSequence != 1 {
		t.Fatalf("restored cursor = %#v, err = %v", restoredCursor, err)
	}
	restoredPresence, err := restarted.GetConversationPresence(ctx, scope, conversation.ID, agent)
	if err != nil || restoredPresence.LeaseID != presence.LeaseID {
		t.Fatalf("restored presence = %#v, err = %v", restoredPresence, err)
	}
	roundReplay, err := restartedService.CoordinateParticipation(ctx, roundRequest)
	if err != nil || !roundReplay.Replayed || len(roundReplay.Messages) != 1 {
		t.Fatalf("restored round replay = %#v, err = %v", roundReplay, err)
	}
	loadedRound, err := restartedService.GetParticipationRound(ctx, scope, conversation.ID, roundReplay.Round.ID)
	if err != nil || len(loadedRound.Messages) != 1 || len(loadedRound.Round.Arbitration.Decisions) != 1 {
		t.Fatalf("loaded round = %#v, err = %v", loadedRound, err)
	}
	rounds, err := restartedService.ListParticipationRounds(ctx, ParticipationRoundFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(rounds) != 1 || rounds[0].Round.ID != roundReplay.Round.ID {
		t.Fatalf("rounds = %#v, err = %v", rounds, err)
	}
	if foreign, err := restartedService.GetConversation(ctx, Scope{Kind: "tenant", ID: "other"}, conversation.ID); err == nil || foreign != nil {
		t.Fatalf("foreign conversation = %#v, err = %v", foreign, err)
	}
	restartedService.now = func() time.Time { return fixed.Add(31 * time.Second) }
	active, err := restartedService.ListPresence(ctx, scope, conversation.ID)
	if err != nil || len(active) != 0 {
		t.Fatalf("expired presence = %#v, err = %v", active, err)
	}
}
