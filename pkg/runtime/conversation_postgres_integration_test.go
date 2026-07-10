//go:build integration

package runtime

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresNaturalChannelsAreConcurrentRestartSafeAndIsolated(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_natural_channels_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}

	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	primaryService := NewConversationService(primary)
	primaryService.now = func() time.Time { return fixed }
	replicaService := NewConversationService(replica)
	replicaService.now = func() time.Time { return fixed }
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation, _, err := primaryService.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"}, Title: "Release channel", IdempotencyKey: "release-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	question, err := primaryService.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Is production ready?", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		References: []ConversationReference{{Kind: ConversationReferenceRun, ID: "release-run"}}, IdempotencyKey: "question-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}
	cursor, _, err := replicaService.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent,
		DeliveredSequence: question.Message.Sequence, ReadSequence: question.Message.Sequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	presence, err := primaryService.SetPresence(ctx, SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: ConversationPresenceWorking,
		Summary: "Validating production", RunID: "release-run", TTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	roundRequest := CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: question.Conversation.Revision,
		TriggerMessageID: question.Message.ID, IdempotencyKey: "round-v1",
		Proposals: []ParticipationProposal{
			{
				Participant: agent, WantsToSpeak: true, Intent: MessageIntentAnswer,
				Content: "Production is ready; rollout and smoke evidence passed.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: question.Message.ID, ResolvesMessageID: question.Message.ID,
				References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "smoke-evidence", Version: 1}},
				Signals:    ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true, HasEvidence: true},
			},
			{
				Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "observer"}, WantsToSpeak: true,
				Intent: MessageIntentAcknowledgment, Content: "Great, thanks everyone.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				Signals: ParticipationSignals{RoleRelevant: true},
			},
		},
	}
	services := []*ConversationService{primaryService, replicaService}
	var committed atomic.Int32
	var failed atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 24; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := services[index%len(services)].CoordinateParticipation(ctx, roundRequest)
			if err != nil {
				failed.Add(1)
				return
			}
			if !result.Replayed {
				committed.Add(1)
			}
		}()
	}
	wait.Wait()
	if failed.Load() != 0 || committed.Load() != 1 {
		t.Fatalf("failed = %d, committed = %d", failed.Load(), committed.Load())
	}

	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
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
	replay, err := restartedService.CoordinateParticipation(ctx, roundRequest)
	if err != nil || !replay.Replayed || len(replay.Messages) != 1 || len(replay.Round.Arbitration.Decisions) != 2 {
		t.Fatalf("round replay = %#v, err = %v", replay, err)
	}
	loadedRound, err := restartedService.GetParticipationRound(ctx, scope, conversation.ID, replay.Round.ID)
	if err != nil || len(loadedRound.Messages) != 1 || len(loadedRound.Round.Arbitration.Decisions) != 2 {
		t.Fatalf("loaded round = %#v, err = %v", loadedRound, err)
	}
	rounds, err := restartedService.ListParticipationRounds(ctx, ParticipationRoundFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(rounds) != 1 || rounds[0].Round.ID != replay.Round.ID {
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
