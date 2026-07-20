package runtime

import (
	"context"
	"errors"
	"strings"
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

func TestConversationCoordinatorNeverExposesTargetedMessageToOtherAgents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := NewConversationService(NewMemoryStore(100))
	scope := Scope{Kind: "tenant", ID: "private"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "launch"}, Title: "Launch", IdempotencyKey: "private-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	developer := ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}
	marketing := ConversationParticipant{Type: ConversationParticipantAgent, ID: "marketing"}
	question, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Is the private security review complete?", Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{developer}},
		RequiresResponse: true, IdempotencyKey: "private-question",
	})
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: developer, SemanticRoles: []string{"developer"}}, {Participant: marketing, SemanticRoles: []string{"marketing"}}}, nil
	})
	var calls atomic.Int32
	provider := ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		calls.Add(1)
		if input.Participant != developer || input.Trigger == nil || input.Trigger.ID != question.Message.ID || len(input.RecentMessages) != 1 || len(input.OpenMessages) != 1 || input.OpenMessages[0].ID != question.Message.ID {
			t.Fatalf("provider received unauthorized context: %#v", input)
		}
		return ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Yes. Security reviewer Alice signed artifact report-17.",
			Audience:         ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{developer}},
			ReplyToMessageID: question.Message.ID, ResolvesMessageID: question.Message.ID,
			Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true}}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, provider, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	round, err := coordinator.Coordinate(ctx, ConversationCoordinationRequest{Scope: scope, ConversationID: conversation.ID,
		ExpectedRevision: question.Conversation.Revision, TriggerMessageID: question.Message.ID, IdempotencyKey: "private-round"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(round.Messages) != 1 || round.Messages[0].Sender != developer || len(round.Round.Proposals) != 2 || round.Round.Proposals[1].Participant != marketing || round.Round.Proposals[1].WantsToSpeak {
		t.Fatalf("calls=%d messages=%#v arbitration=%#v proposals=%#v", calls.Load(), round.Messages, round.Round.Arbitration, round.Round.Proposals)
	}
	developerCursor, err := service.GetCursor(ctx, scope, conversation.ID, developer)
	if err != nil || developerCursor == nil || developerCursor.ReadSequence != question.Message.Sequence {
		t.Fatalf("developer cursor=%#v err=%v", developerCursor, err)
	}
	marketingCursor, err := service.GetCursor(ctx, scope, conversation.ID, marketing)
	if err != nil || marketingCursor != nil {
		t.Fatalf("marketing cursor=%#v err=%v", marketingCursor, err)
	}
}

func TestOpenConversationMessagesTracksDurableResolution(t *testing.T) {
	question := &ChannelMessage{ID: "question", Intent: MessageIntentQuestion, RequiresResponse: true, Sequence: 1}
	proposal := &ChannelMessage{ID: "proposal", Intent: MessageIntentProposal, RequiresResponse: true, Sequence: 2}
	answer := &ChannelMessage{ID: "answer", Intent: MessageIntentAnswer, ResolvesMessageID: question.ID, Sequence: 3}
	open := openConversationMessages([]*ChannelMessage{question, proposal, answer})
	if len(open) != 1 || open[0].ID != proposal.ID {
		t.Fatalf("open messages=%#v", open)
	}
	unlinked := ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Signals: ParticipationSignals{AnswersOpenQuestion: true, ResolvesOpenWork: true}}
	if proposalTargetsOpenMessage(unlinked, open, MessageIntentQuestion, false) || proposalTargetsOpenMessage(unlinked, open, "", true) {
		t.Fatal("unlinked model claims must not resolve durable open work")
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
	if !errors.Is(err, ErrConversationParticipationQuorum) {
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

func TestConversationCoordinatorIsolatesUnavailableParticipantAndPreservesHealthyWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := NewConversationService(NewMemoryStore(100))
	scope := Scope{Kind: "tenant", ID: "degraded"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "ops"}, Title: "Operations", IdempotencyKey: "degraded-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "broken"}, SemanticRoles: []string{"observer"}},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "healthy"}, SemanticRoles: []string{"operator"}},
		}, nil
	})
	healthyStarted := make(chan struct{})
	healthyFinished := make(chan struct{})
	provider := ParticipationProposalProviderFunc(func(ctx context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		if input.Participant.ID == "broken" {
			<-healthyStarted
			return ParticipationProposal{}, errors.New("provider secret sk-must-not-persist")
		}
		close(healthyStarted)
		select {
		case <-time.After(40 * time.Millisecond):
			close(healthyFinished)
		case <-ctx.Done():
			t.Fatalf("healthy participant was canceled: %v", ctx.Err())
		}
		return ParticipationProposal{
			WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "The healthy operator completed the review.",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			Signals:  ParticipationSignals{HasNewInformation: true, RoleRelevant: true},
		}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, provider, ConversationCoordinatorConfig{
		MaximumParticipants: 4, MaximumConcurrency: 2, RecentMessageLimit: 10,
		ProposalTimeout: time.Second, PresenceTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	round, err := coordinator.Coordinate(ctx, ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		MaximumConcurrency: 2, IdempotencyKey: "degraded-round",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-healthyFinished:
	default:
		t.Fatal("healthy participant did not finish")
	}
	if len(round.Messages) != 1 || round.Messages[0].Sender.ID != "healthy" || len(round.Round.Proposals) != 2 {
		t.Fatalf("degraded round = %#v", round)
	}
	broken := round.Round.Proposals[0]
	if broken.Participant.ID != "broken" || broken.Availability.Status != ParticipationUnavailable ||
		broken.Availability.FailureCode != "participant_runtime_unavailable" || broken.WantsToSpeak {
		t.Fatalf("unavailable participant proposal = %#v", broken)
	}
	encoded := broken.Availability.FailureCode + broken.Content
	if strings.Contains(encoded, "secret") || strings.Contains(encoded, "sk-") {
		t.Fatalf("provider details leaked into round: %q", encoded)
	}
	if round.Round.Proposals[1].Availability.Status != ParticipationAvailable {
		t.Fatalf("healthy availability = %#v", round.Round.Proposals[1].Availability)
	}
	replay, err := coordinator.Coordinate(ctx, ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		MaximumConcurrency: 2, IdempotencyKey: "degraded-round",
	})
	if err != nil || !replay.Replayed || len(replay.Messages) != 1 {
		t.Fatalf("degraded replay = %#v, %v", replay, err)
	}
}

func TestConversationCoordinatorEnforcesExplicitRequiredRoleQuorum(t *testing.T) {
	t.Parallel()
	service := NewConversationService(NewMemoryStore(100))
	scope := Scope{Kind: "tenant", ID: "required-role"}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "ops"}, Title: "Operations", IdempotencyKey: "required-role-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewConversationCoordinator(service,
		ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return []ConversationParticipantBinding{
				{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"}, SemanticRoles: []string{"reviewer"}},
				{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "operator"}, SemanticRoles: []string{"operator"}},
			}, nil
		}),
		ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
			if input.Participant.ID == "reviewer" {
				return ParticipationProposal{}, ErrTurnHostUnavailable
			}
			return ParticipationProposal{WantsToSpeak: false}, nil
		}),
		ConversationCoordinatorConfig{MaximumParticipants: 4, MaximumConcurrency: 2, RecentMessageLimit: 10, ProposalTimeout: time.Second, PresenceTTL: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultConversationArbitrationPolicy()
	policy.RequiredAvailableRole = "reviewer"
	_, err = coordinator.Coordinate(t.Context(), ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Policy: policy, IdempotencyKey: "required-role-round",
	})
	if !errors.Is(err, ErrConversationParticipationQuorum) {
		t.Fatalf("required-role quorum error = %v", err)
	}
	rounds, listErr := service.ListParticipationRounds(t.Context(), ParticipationRoundFilter{Scope: scope, ConversationID: conversation.ID})
	if listErr != nil || len(rounds) != 0 {
		t.Fatalf("required-role partial rounds = %#v, %v", rounds, listErr)
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
