//go:build integration

package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostgresNaturalChannelsAreConcurrentRestartSafeAndIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	primary, pgSchema := openTestPostgresStore(t, "openseal_natural_channels_")
	schema := pgSchema.name
	dsn := pgSchema.dsn
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}

	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	primaryService := NewConversationService(primary)
	primaryService.now = func() time.Time { return fixed }
	replicaService := NewConversationService(replica)
	replicaService.now = func() time.Time { return fixed }
	scope := Scope{Kind: "tenant", ID: "one"}
	seedTestSpace(t, primary, scope)
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
	roundRequest := CommitConversationRoutingRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: question.Conversation.Revision,
		TriggerMessageID: question.Message.ID, IdempotencyKey: "round-v1",
		Responders: []ConversationResponder{
			{Participant: agent, Addressed: ConversationAddressedRouted, Reason: "owns production"},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "observer"}, Addressed: ConversationAddressedEveryone},
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
			result, err := services[index%len(services)].CommitConversationRouting(ctx, roundRequest)
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
	if err != nil || restored.Revision != 3 || restored.LastSequence != 1 {
		t.Fatalf("restored conversation = %#v, err = %v", restored, err)
	}
	messages, err := restartedService.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 1 {
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
	replay, err := restartedService.CommitConversationRouting(ctx, roundRequest)
	if err != nil || !replay.Replayed || len(replay.Messages) != 0 || len(replay.Round.Responders) != 2 {
		t.Fatalf("round replay = %#v, err = %v", replay, err)
	}
	loadedRound, err := restartedService.GetParticipationRound(ctx, scope, conversation.ID, replay.Round.ID)
	if err != nil || len(loadedRound.Round.Responders) != 2 || loadedRound.Round.Responders[0].Reason != "owns production" {
		t.Fatalf("loaded round = %#v, err = %v", loadedRound, err)
	}
	rounds, err := restartedService.ListParticipationRounds(ctx, ParticipationRoundFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(rounds) != 1 || rounds[0].Round.ID != replay.Round.ID || rounds[0].Round.ConversationRevision != 3 {
		t.Fatalf("rounds = %#v, err = %v", rounds, err)
	}
	primaryChanges, err := NewConversationChangeService(primary, primary)
	if err != nil {
		t.Fatal(err)
	}
	initialChanges, err := primaryChanges.ListChanges(ctx, ConversationChangeRequest{
		Scope: scope, ConversationID: conversation.ID, ActiveAt: fixed,
	})
	if err != nil || len(initialChanges.Messages) != 1 || len(initialChanges.Rounds) != 1 ||
		len(initialChanges.Presence) != 1 || initialChanges.Cursor == "" {
		t.Fatalf("initial PostgreSQL changes = %#v, err = %v", initialChanges, err)
	}
	replicaChanges, err := NewConversationChangeService(replica, replica)
	if err != nil {
		t.Fatal(err)
	}
	unchangedChanges, err := replicaChanges.ListChanges(ctx, ConversationChangeRequest{
		Scope: scope, ConversationID: conversation.ID, Cursor: initialChanges.Cursor, ActiveAt: fixed,
	})
	if err != nil || unchangedChanges.HasChanges {
		t.Fatalf("replica cursor replay = %#v, err = %v", unchangedChanges, err)
	}
	expiredChanges, err := replicaChanges.ListChanges(ctx, ConversationChangeRequest{
		Scope: scope, ConversationID: conversation.ID, Cursor: initialChanges.Cursor, ActiveAt: fixed.Add(31 * time.Second),
	})
	if err != nil || !expiredChanges.HasChanges || len(expiredChanges.Presence) != 0 {
		t.Fatalf("replica presence expiry = %#v, err = %v", expiredChanges, err)
	}
	recoveryConversation, _, err := primaryService.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"}, Title: "Recovery channel", IdempotencyKey: "recovery-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveryMessage, err := primaryService.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: recoveryConversation.ID, ExpectedRevision: recoveryConversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Which replica owns this wake?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "recovery-question",
	})
	if err != nil {
		t.Fatal(err)
	}
	primaryScheduler, err := NewConversationRunScheduler(primary, primary, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	replicaScheduler, err := NewConversationRunScheduler(replica, replica, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	schedulers := []*ConversationRunScheduler{primaryScheduler, replicaScheduler}
	reconcileErrors := make(chan error, len(schedulers))
	wait = sync.WaitGroup{}
	for index := range schedulers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, reconcileErr := schedulers[index].ReconcileScope(ctx, scope)
			reconcileErrors <- reconcileErr
		}(index)
	}
	wait.Wait()
	close(reconcileErrors)
	for reconcileErr := range reconcileErrors {
		if reconcileErr != nil {
			t.Fatal(reconcileErr)
		}
	}
	conversationRuns, err := restarted.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
	if err != nil || len(conversationRuns) != 1 || conversationRuns[0].ConcurrencyKey != recoveryConversation.ID ||
		conversationRuns[0].Context[conversationRunContextTriggerID] != recoveryMessage.Message.ID {
		t.Fatalf("cross-replica reconciled Runs = %#v, err = %v", conversationRuns, err)
	}
	restartedChanges, err := NewConversationChangeService(restarted, restarted)
	if err != nil {
		t.Fatal(err)
	}
	recoveryChanges, err := restartedChanges.ListChanges(ctx, ConversationChangeRequest{
		Scope: scope, ConversationID: recoveryConversation.ID, ActiveAt: fixed,
	})
	if err != nil || !recoveryChanges.RunsChanged || !recoveryChanges.ActivityChanged ||
		len(recoveryChanges.Runs) != 1 || recoveryChanges.Runs[0].ID != conversationRuns[0].ID ||
		len(recoveryChanges.Activity) != 1 || recoveryChanges.Activity[0].RunID != conversationRuns[0].ID ||
		recoveryChanges.Activity[0].EventType != "run.created" {
		t.Fatalf("restart-safe Run activity projection = %#v, err = %v", recoveryChanges, err)
	}
	if foreign, err := restartedService.GetConversation(ctx, Scope{Kind: "tenant", ID: "other"}, conversation.ID); err == nil || foreign != nil {
		t.Fatalf("foreign conversation = %#v, err = %v", foreign, err)
	}
	restartedService.now = func() time.Time { return fixed.Add(31 * time.Second) }
	active, err := restartedService.ListPresence(ctx, scope, conversation.ID)
	if err != nil || len(active) != 0 {
		t.Fatalf("expired presence = %#v, err = %v", active, err)
	}
	private, err := restartedService.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: restored.Revision,
		Sender: agent, Intent: MessageIntentUpdate, Content: "Private deployment detail",
		Audience:       ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "reviewer"}}},
		IdempotencyKey: "private-deployment-detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	operator := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}}
	descendingVisible, err := replicaService.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: scope, ConversationID: conversation.ID, Descending: true, Limit: 1, Viewer: &operator,
	})
	if err != nil || len(descendingVisible) != 1 || descendingVisible[0].Sequence != private.Message.Sequence-1 {
		t.Fatalf("PostgreSQL descending audience page = %#v, err = %v", descendingVisible, err)
	}
	olderVisible, err := replicaService.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: scope, ConversationID: conversation.ID, Descending: true, BeforeSequence: descendingVisible[0].Sequence, Limit: 1, Viewer: &operator,
	})
	if err != nil || len(olderVisible) != 1 || olderVisible[0].Sequence != 1 {
		t.Fatalf("PostgreSQL before-sequence page = %#v, err = %v", olderVisible, err)
	}
}
