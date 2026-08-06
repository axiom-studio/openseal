package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConversationServiceCreatesObjectiveScopedConversation(t *testing.T) {
	t.Parallel()
	service := NewConversationService(NewMemoryStore())
	origin := &ConversationReference{Kind: ConversationReferenceObjective, ID: "objective-reddit-research"}

	conversation, replayed, err := service.CreateConversation(context.Background(), CreateConversationRequest{
		Scope:          Scope{Kind: "tenant", ID: "one"},
		Owner:          ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"},
		Title:          "Monitor Reddit pain points",
		Origin:         origin,
		IdempotencyKey: "objective-conversation",
	})
	if err != nil || replayed {
		t.Fatalf("create objective conversation: replayed=%v err=%v", replayed, err)
	}
	if conversation.Origin == nil || conversation.Origin.Kind != ConversationReferenceObjective || conversation.Origin.ID != origin.ID {
		t.Fatalf("objective origin = %#v", conversation.Origin)
	}
}

func TestConversationServiceCoordinatesNaturalDurableRound(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	service := NewConversationService(store)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "product"}
	conversation, replayed, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Release coordination", IdempotencyKey: "release-channel-v1",
	})
	if err != nil || replayed || conversation.Revision != 1 {
		t.Fatalf("conversation = %#v, replayed = %v, err = %v", conversation, replayed, err)
	}
	duplicate, replayed, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Release coordination", IdempotencyKey: "release-channel-v1",
	})
	if err != nil || !replayed || duplicate.ID != conversation.ID {
		t.Fatalf("conversation replay = %#v, replayed = %v, err = %v", duplicate, replayed, err)
	}

	user := ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}
	developer := ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}
	question, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: user, Intent: MessageIntentQuestion, Content: "Is the release ready?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, References: []ConversationReference{{Kind: ConversationReferenceRun, ID: "release-run"}},
		IdempotencyKey: "release-question-v1",
	})
	if err != nil || question.Message.Sequence != 1 || question.Conversation.Revision != 2 {
		t.Fatalf("question = %#v, err = %v", question, err)
	}
	questionReplay, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: 1,
		Sender: user, Intent: MessageIntentQuestion, Content: "Is the release ready?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, References: []ConversationReference{{Kind: ConversationReferenceRun, ID: "release-run"}},
		IdempotencyKey: "release-question-v1",
	})
	if err != nil || !questionReplay.Replayed || questionReplay.Message.ID != question.Message.ID {
		t.Fatalf("question replay = %#v, err = %v", questionReplay, err)
	}

	round, err := service.CoordinateParticipation(ctx, CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: question.Conversation.Revision,
		TriggerMessageID: question.Message.ID, IdempotencyKey: "release-round-v1",
		Policy: ConversationArbitrationPolicy{MinimumScore: 30, MaximumSpeakers: 3, DuplicateThreshold: 0.7},
		Proposals: []ParticipationProposal{
			{
				Participant: developer, WantsToSpeak: true, Intent: MessageIntentAnswer,
				Content: "The release is ready; deployment and smoke-test evidence are linked.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: question.Message.ID, BroadcastToChannel: true, ResolvesMessageID: question.Message.ID,
				References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "smoke-evidence", Version: 1}},
				Signals:    ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true, HasEvidence: true, ResolvesOpenWork: true},
			},
			{
				Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "marketing"}, WantsToSpeak: true,
				Intent: MessageIntentUpdate, Content: "Deployment and smoke-test evidence are linked; the release is ready.",
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true},
			},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "analyst"}, WantsToSpeak: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Messages) != 1 || round.Messages[0].Sender != developer || round.Messages[0].Sequence != 2 ||
		round.Messages[0].ThreadRootID != question.Message.ID || !round.Messages[0].BroadcastToChannel ||
		round.Messages[0].ResolvesMessageID != question.Message.ID ||
		round.Conversation.LastSequence != 2 || round.Conversation.Revision != 3 {
		t.Fatalf("round = %#v", round)
	}
	decisions := decisionsByProposal(round.Round.Arbitration.Decisions)
	if len(decisions) != 3 {
		t.Fatalf("round decisions = %#v", decisions)
	}
	roundReplay, err := service.CoordinateParticipation(ctx, CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: 2, TriggerMessageID: question.Message.ID,
		IdempotencyKey: "release-round-v1", Proposals: round.Round.Proposals, Policy: round.Round.Policy,
	})
	if err != nil || !roundReplay.Replayed || roundReplay.Round.ID != round.Round.ID {
		t.Fatalf("round replay = %#v, err = %v", roundReplay, err)
	}
	conflictingProposals := cloneParticipationProposals(round.Round.Proposals)
	conflictingProposals[0].Content = "A conflicting answer under the same key."
	if _, err := service.CoordinateParticipation(ctx, CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: 2, TriggerMessageID: question.Message.ID,
		IdempotencyKey: "release-round-v1", Proposals: conflictingProposals, Policy: round.Round.Policy,
	}); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("conflicting round error = %v", err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[0].Intent != MessageIntentQuestion || messages[1].Intent != MessageIntentAnswer {
		t.Fatalf("messages = %#v, err = %v", messages, err)
	}
	thread, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, ThreadRootID: question.Message.ID})
	if err != nil || len(thread) != 2 {
		t.Fatalf("thread = %#v, err = %v", thread, err)
	}
}

func TestConversationAudienceVisibilityIsFailClosedAndThreadSafe(t *testing.T) {
	service := NewConversationService(NewMemoryStore())
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "visibility"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Private coordination", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	sender := ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}
	target := ConversationParticipant{Type: ConversationParticipantAgent, ID: "target"}
	other := ConversationParticipant{Type: ConversationParticipantAgent, ID: "other"}
	direct, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: sender, Intent: MessageIntentQuestion, Content: "private question", Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{target}}, IdempotencyKey: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: direct.Conversation.Revision, Sender: target, Intent: MessageIntentAnswer, Content: "public-looking reply", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: direct.Message.ID, IdempotencyKey: "reply"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PostChannelMessage(ctx, PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: reply.Conversation.Revision, Sender: sender, Intent: MessageIntentUpdate, Content: "operators only", Audience: ConversationAudience{Kind: ConversationAudienceRoles, Roles: []string{"operator"}}, IdempotencyKey: "role"})
	if err != nil {
		t.Fatal(err)
	}
	for name, viewer := range map[string]struct {
		viewer ConversationViewer
		want   int
	}{
		"sender": {ConversationViewer{Participant: sender}, 3},
		"target": {ConversationViewer{Participant: target}, 2},
		"other":  {ConversationViewer{Participant: other}, 0},
		"role":   {ConversationViewer{Participant: other, Roles: []string{"operator"}}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Viewer: &viewer.viewer})
			if err != nil || len(messages) != viewer.want {
				t.Fatalf("messages=%#v err=%v", messages, err)
			}
		})
	}
	if _, err := service.GetVisibleChannelMessage(ctx, scope, conversation.ID, direct.Message.ID, ConversationViewer{Participant: other}); !errors.Is(err, ErrChannelMessageNotFound) {
		t.Fatalf("hidden get error=%v", err)
	}
	if _, err := service.GetVisibleChannelMessage(ctx, scope, conversation.ID, direct.Message.ID, ConversationViewer{Participant: target}); err != nil {
		t.Fatalf("target get=%v", err)
	}
	paged, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Limit: 1, Viewer: &ConversationViewer{Participant: other, Roles: []string{"operator"}}})
	if err != nil || len(paged) != 1 || paged[0].Content != "operators only" {
		t.Fatalf("hidden-page scan=%#v err=%v", paged, err)
	}
	descending, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Limit: 1, Descending: true, Viewer: &ConversationViewer{Participant: target}})
	if err != nil || len(descending) != 1 || descending[0].ID != reply.Message.ID {
		t.Fatalf("descending hidden-page scan=%#v err=%v", descending, err)
	}
	bounded, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, BeforeSequence: reply.Message.Sequence, Viewer: &ConversationViewer{Participant: target}})
	if err != nil || len(bounded) != 1 || bounded[0].ID != direct.Message.ID {
		t.Fatalf("before-sequence page=%#v err=%v", bounded, err)
	}
	if _, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, AfterSequence: 2, BeforeSequence: 2}); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("invalid sequence bounds error=%v", err)
	}
}

func TestConversationServicePersistsAQuietRound(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	service := NewConversationService(store)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "quiet"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Quiet channel", IdempotencyKey: "quiet-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	round, err := service.CoordinateParticipation(ctx, CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, IdempotencyKey: "quiet-round-v1",
		Proposals: []ParticipationProposal{
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "one"}, WantsToSpeak: false},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "two"}, WantsToSpeak: false},
		},
	})
	if err != nil || len(round.Messages) != 0 || len(round.Round.Arbitration.Decisions) != 2 || round.Conversation.Revision != 2 || round.Conversation.LastSequence != 0 {
		t.Fatalf("quiet round = %#v, err = %v", round, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 0 {
		t.Fatalf("quiet messages = %#v, err = %v", messages, err)
	}
}

func TestConversationServicePersistsMonotonicReceiptsAndTruthfulPresence(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	service := NewConversationService(store)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "product"}, Title: "Operations", IdempotencyKey: "operations-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentUpdate,
		Content: "Investigate the alert.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "alert-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: "sre"}
	cursor, replayed, err := service.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, DeliveredSequence: message.Message.Sequence, ReadSequence: 0,
	})
	if err != nil || replayed || cursor.Revision != 1 {
		t.Fatalf("cursor = %#v, replayed = %v, err = %v", cursor, replayed, err)
	}
	cursor, replayed, err = service.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, ExpectedRevision: cursor.Revision,
		DeliveredSequence: message.Message.Sequence, ReadSequence: message.Message.Sequence,
	})
	if err != nil || replayed || cursor.Revision != 2 {
		t.Fatalf("read cursor = %#v, replayed = %v, err = %v", cursor, replayed, err)
	}
	if _, _, err := service.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, ExpectedRevision: cursor.Revision,
		DeliveredSequence: 0, ReadSequence: 0,
	}); !errors.Is(err, ErrConversationCursorConflict) {
		t.Fatalf("cursor regression error = %v", err)
	}
	user := ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}
	if _, _, err := service.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: user,
		DeliveredSequence: message.Message.Sequence, ReadSequence: 0,
	}); err != nil {
		t.Fatal(err)
	}
	cursors, err := service.ListCursors(ctx, scope, conversation.ID)
	if err != nil || len(cursors) != 2 {
		t.Fatalf("cursor list = %#v, err = %v", cursors, err)
	}
	if cursors[0].Participant != agent || cursors[1].Participant != user {
		t.Fatalf("cursor list is not deterministically ordered: %#v", cursors)
	}

	presence, err := service.SetPresence(ctx, SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: ConversationPresenceWorking,
		Summary: "Inspecting alert evidence", RunID: "alert-run", TTL: 30 * time.Second,
	})
	if err != nil || presence.LeaseID == "" || presence.Revision != 1 {
		t.Fatalf("presence = %#v, err = %v", presence, err)
	}
	if _, err := service.SetPresence(ctx, SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: ConversationPresenceTyping,
		LeaseID: "another-lease", ExpectedRevision: presence.Revision, TTL: 30 * time.Second,
	}); !errors.Is(err, ErrConversationPresenceConflict) {
		t.Fatalf("foreign lease error = %v", err)
	}
	presence, err = service.SetPresence(ctx, SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: ConversationPresenceTyping,
		Summary: "Writing update", LeaseID: presence.LeaseID, ExpectedRevision: presence.Revision, TTL: 20 * time.Second,
	})
	if err != nil || presence.Revision != 2 {
		t.Fatalf("renewed presence = %#v, err = %v", presence, err)
	}
	active, err := service.ListPresence(ctx, scope, conversation.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("active presence = %#v, err = %v", active, err)
	}
	now = now.Add(21 * time.Second)
	active, err = service.ListPresence(ctx, scope, conversation.ID)
	if err != nil || len(active) != 0 {
		t.Fatalf("expired presence = %#v, err = %v", active, err)
	}
	replacement, err := service.SetPresence(ctx, SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: ConversationPresenceThinking, TTL: 30 * time.Second,
	})
	if err != nil || replacement.Revision != 1 || replacement.LeaseID == presence.LeaseID {
		t.Fatalf("replacement presence = %#v, err = %v", replacement, err)
	}
	if err := service.ReleasePresence(ctx, ReleaseConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, LeaseID: replacement.LeaseID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConversationParticipationRoundIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	service := NewConversationService(store)
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixed }
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "concurrent"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Concurrent", IdempotencyKey: "concurrent-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, IdempotencyKey: "round-v1",
		Proposals: []ParticipationProposal{{
			Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}, WantsToSpeak: true,
			Intent: MessageIntentUpdate, Content: "One durable update.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true},
		}},
	}
	var committed atomic.Int32
	var failed atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.CoordinateParticipation(ctx, request)
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
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 1 || messages[0].Sequence != 1 {
		t.Fatalf("messages = %#v, err = %v", messages, err)
	}
}
