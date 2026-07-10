package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConversationCoordinatorRunsGovernedNaturalRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(100)
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "launch"}, Title: "Launch readiness", IdempotencyKey: "launch-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	developer := ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}
	question, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Is the launch ready?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		Mentions: []ConversationParticipant{developer}, RequiresResponse: true, IdempotencyKey: "launch-question",
	})
	if err != nil {
		t.Fatal(err)
	}

	var sourceCalls atomic.Int32
	participants := ConversationParticipantSourceFunc(func(_ context.Context, query ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		sourceCalls.Add(1)
		if query.Conversation.ID != conversation.ID || query.Trigger.ID != question.Message.ID {
			t.Fatalf("participant query = %#v", query)
		}
		return []ConversationParticipantBinding{
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "marketing"}, SemanticRoles: []string{"marketing"}},
			{Participant: developer, SemanticRoles: []string{"developer"}, Priority: 5},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "analyst"}, SemanticRoles: []string{"analyst"}},
		}, nil
	})

	var providerCalls atomic.Int32
	var active atomic.Int32
	var maximumActive atomic.Int32
	var contextsMu sync.Mutex
	contexts := make(map[string]ParticipationProposalContext)
	provider := ParticipationProposalProviderFunc(func(ctx context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		providerCalls.Add(1)
		current := active.Add(1)
		for {
			maximum := maximumActive.Load()
			if current <= maximum || maximumActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		defer active.Add(-1)
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return ParticipationProposal{}, ctx.Err()
		}
		contextsMu.Lock()
		contexts[input.Participant.ID] = input
		contextsMu.Unlock()
		switch input.Participant.ID {
		case "developer":
			return ParticipationProposal{
				Participant:   ConversationParticipant{Type: ConversationParticipantUser, ID: "spoofed"},
				SemanticRoles: []string{"leader"}, Priority: 100, WantsToSpeak: true, Intent: MessageIntentAnswer,
				Content: "The launch is ready and the smoke evidence passed.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: question.Message.ID, Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, HasEvidence: true},
			}, nil
		case "marketing":
			return ParticipationProposal{
				WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "The launch is ready and the smoke evidence passed.",
				Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				Signals:  ParticipationSignals{DirectlyMentioned: true, HasNewInformation: true, RoleRelevant: true},
			}, nil
		default:
			return ParticipationProposal{WantsToSpeak: false}, nil
		}
	})
	coordinator, err := NewConversationCoordinator(service, participants, provider, ConversationCoordinatorConfig{
		MaximumParticipants: 8, MaximumConcurrency: 4, RecentMessageLimit: 20,
		ProposalTimeout: time.Second, PresenceTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: question.Conversation.Revision,
		TriggerMessageID: question.Message.ID, MaximumConcurrency: 2, IdempotencyKey: "launch-round",
	}
	round, err := coordinator.Coordinate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCalls.Load() != 1 || providerCalls.Load() != 3 || maximumActive.Load() != 2 {
		t.Fatalf("source calls = %d, provider calls = %d, max concurrency = %d", sourceCalls.Load(), providerCalls.Load(), maximumActive.Load())
	}
	if len(round.Messages) != 1 || round.Messages[0].Sender != developer || round.Messages[0].Content != "The launch is ready and the smoke evidence passed." {
		t.Fatalf("coordinated messages = %#v", round.Messages)
	}
	if len(round.Round.Proposals) != 3 || round.Round.Proposals[0].Participant.ID != "analyst" ||
		round.Round.Proposals[1].Participant != developer || round.Round.Proposals[1].Priority != 5 ||
		len(round.Round.Proposals[1].SemanticRoles) != 1 || round.Round.Proposals[1].SemanticRoles[0] != "developer" ||
		!round.Round.Proposals[1].Signals.DirectlyMentioned || round.Round.Proposals[1].Signals.TriggerTargetsOtherParticipant ||
		!round.Round.Proposals[0].Signals.TriggerTargetsOtherParticipant || !round.Round.Proposals[2].Signals.TriggerTargetsOtherParticipant {
		t.Fatalf("governed proposals = %#v", round.Round.Proposals)
	}
	var marketingDecision *ParticipationDecision
	for index := range round.Round.Arbitration.Decisions {
		if round.Round.Arbitration.Decisions[index].Participant.ID == "marketing" {
			marketingDecision = &round.Round.Arbitration.Decisions[index]
		}
	}
	if marketingDecision == nil || marketingDecision.Disposition != ParticipationSilent ||
		!containsParticipationReason(marketingDecision.Reasons, ParticipationReasonNotAddressed) {
		t.Fatalf("marketing decision = %#v", marketingDecision)
	}
	if contexts["developer"].Trigger.ID != question.Message.ID || len(contexts["developer"].RecentMessages) != 1 ||
		contexts["developer"].RecentMessages[0].ID != question.Message.ID {
		t.Fatalf("developer proposal context = %#v", contexts["developer"])
	}
	for _, participant := range []ConversationParticipant{
		{Type: ConversationParticipantAgent, ID: "analyst"}, developer,
		{Type: ConversationParticipantAgent, ID: "marketing"},
	} {
		cursor, cursorErr := service.GetCursor(ctx, scope, conversation.ID, participant)
		if cursorErr != nil || cursor == nil || cursor.ReadSequence != question.Message.Sequence || cursor.DeliveredSequence != question.Message.Sequence {
			t.Fatalf("cursor for %s = %#v, err = %v", participant.ID, cursor, cursorErr)
		}
	}
	if presence, presenceErr := service.ListPresence(ctx, scope, conversation.ID); presenceErr != nil || len(presence) != 0 {
		t.Fatalf("presence after round = %#v, err = %v", presence, presenceErr)
	}

	replay, err := coordinator.Coordinate(ctx, request)
	if err != nil || !replay.Replayed || replay.Round.ID != round.Round.ID || sourceCalls.Load() != 1 || providerCalls.Load() != 3 {
		t.Fatalf("replay = %#v, source calls = %d, provider calls = %d, err = %v", replay, sourceCalls.Load(), providerCalls.Load(), err)
	}
}

func TestConversationCoordinatorFailsClosedWithoutCommittingPartialRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(100)
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "failure"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "ops"}, Title: "Operations", IdempotencyKey: "ops-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: "sre"}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: agent, SemanticRoles: []string{"sre"}}}, nil
	})
	providerErr := errors.New("model temporarily unavailable")
	coordinator, err := NewConversationCoordinator(service, participants, ParticipationProposalProviderFunc(
		func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
			return ParticipationProposal{}, providerErr
		}), ConversationCoordinatorConfig{
		MaximumParticipants: 4, MaximumConcurrency: 1, RecentMessageLimit: 10,
		ProposalTimeout: time.Second, PresenceTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Coordinate(ctx, ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, IdempotencyKey: "failed-round",
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("coordination error = %v", err)
	}
	rounds, listErr := service.ListParticipationRounds(ctx, ParticipationRoundFilter{Scope: scope, ConversationID: conversation.ID})
	if listErr != nil || len(rounds) != 0 {
		t.Fatalf("partial rounds = %#v, err = %v", rounds, listErr)
	}
	if presence, presenceErr := service.ListPresence(ctx, scope, conversation.ID); presenceErr != nil || len(presence) != 0 {
		t.Fatalf("presence after failure = %#v, err = %v", presence, presenceErr)
	}
}

func TestConversationCoordinatorRejectsStaleProposalsAfterChannelDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore(100)
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "drift"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"}, Title: "Engineering", IdempotencyKey: "engineering-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}
	coordinator, err := NewConversationCoordinator(service,
		ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return []ConversationParticipantBinding{{Participant: agent, SemanticRoles: []string{"developer"}}}, nil
		}),
		ParticipationProposalProviderFunc(func(ctx context.Context, _ ParticipationProposalContext) (ParticipationProposal, error) {
			_, postErr := service.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentUpdate,
				Content: "A newer decision arrived while the Agent was working.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				IdempotencyKey: "newer-decision",
			})
			if postErr != nil {
				return ParticipationProposal{}, postErr
			}
			return ParticipationProposal{
				WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "My now-stale proposal.",
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true},
			}, nil
		}),
		ConversationCoordinatorConfig{
			MaximumParticipants: 4, MaximumConcurrency: 1, RecentMessageLimit: 10,
			ProposalTimeout: time.Second, PresenceTTL: 30 * time.Second,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Coordinate(ctx, ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, IdempotencyKey: "stale-round",
	})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("coordination error = %v", err)
	}
	rounds, listErr := service.ListParticipationRounds(ctx, ParticipationRoundFilter{Scope: scope, ConversationID: conversation.ID})
	if listErr != nil || len(rounds) != 0 {
		t.Fatalf("stale rounds = %#v, err = %v", rounds, listErr)
	}
	messages, listErr := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if listErr != nil || len(messages) != 1 || messages[0].Content != "A newer decision arrived while the Agent was working." {
		t.Fatalf("messages after drift = %#v, err = %v", messages, listErr)
	}
}
