package runtime

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestMeteredParticipationChargesAllProposalsAndNotReplay(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "local", ID: "usage"}
	channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, Title: "Research", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review the evidence", "trigger")
	channel, err = service.GetConversation(t.Context(), scope, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "a"}, SemanticRoles: []string{"reviewer"}},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "b"}, SemanticRoles: []string{"reviewer"}},
		}, nil
	})
	var calls atomic.Int32
	provider := MeteredParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (MeteredParticipationProposal, error) {
		calls.Add(1)
		report := MeteredParticipationProposal{Usage: TurnUsage{InputTokens: 10, OutputTokens: 5, Cost: 0.125, DurationMS: 100, ProviderDurationMS: 80}, Proposal: ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "The evidence needs review.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true}}}
		if input.Participant.ID == "b" {
			return report, errors.New("provider validation failed after generation")
		}
		return report, nil
	})
	config := DefaultConversationCoordinatorConfig()
	config.RequireUsageReporting = true
	coordinator, err := NewConversationCoordinator(service, participants, provider, config)
	if err != nil {
		t.Fatal(err)
	}
	req := ConversationCoordinationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, TriggerMessageID: trigger.ID, IdempotencyKey: "round"}
	result, usage, err := coordinator.CoordinateWithUsage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || usage.InputTokens != 20 || usage.OutputTokens != 10 || usage.Cost != 0.25 || usage.DurationMS != 100 || usage.ProviderDurationMS != 160 || calls.Load() != 2 {
		t.Fatalf("metered round: %#v usage=%#v calls=%d", result, usage, calls.Load())
	}
	replay, usage, err := coordinator.CoordinateWithUsage(t.Context(), req)
	if err != nil || !replay.Replayed || usage != (TurnUsage{}) || calls.Load() != 2 {
		t.Fatalf("replay charged: %#v usage=%#v err=%v calls=%d", replay, usage, err, calls.Load())
	}
}

func TestMeteredParticipationRetainsUsageWhenPublicationConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metered.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := NewConversationService(store)
	scope := Scope{Kind: "local", ID: "conflict"}
	channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Research", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review evidence", "trigger")
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "a"}, SemanticRoles: []string{"reviewer"}}}, nil
	})
	provider := MeteredParticipationProposalProviderFunc(func(ctx context.Context, input ParticipationProposalContext) (MeteredParticipationProposal, error) {
		title := "Changed during generation"
		_, err := service.UpdateConversation(ctx, UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: input.Conversation.Revision, Title: &title})
		return MeteredParticipationProposal{Usage: TurnUsage{InputTokens: 19, OutputTokens: 7}, Proposal: ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Late reply", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true}}}, err
	})
	coordinator, err := NewConversationCoordinator(service, participants, provider, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	request := conversationAgentRunRequest(channel, trigger)
	request.Budget = &BudgetPolicy{MaxTurns: 4, MaxTotalTokens: 1000}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != AgentRunStatusSleeping || result.Turn.Usage.InputTokens != 19 || result.Turn.Usage.OutputTokens != 7 || result.Run.BudgetUsage.InputTokens != 19 || result.Run.BudgetUsage.OutputTokens != 7 {
		t.Fatalf("retry lost usage: %#v", result)
	}
	messages, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: channel.ID})
	if err != nil || len(messages) != 1 {
		t.Fatalf("stale reply published: %#v %v", messages, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.GetAgentRun(t.Context(), scope, run.ID)
	if err != nil || saved.BudgetUsage.InputTokens != 19 || saved.BudgetUsage.OutputTokens != 7 {
		t.Fatalf("restart lost usage: %#v %v", saved, err)
	}
}

func TestParticipationUsageRejectsOverflowAndInvalidReports(t *testing.T) {
	for _, test := range []struct{ total, next TurnUsage }{
		{TurnUsage{InputTokens: math.MaxInt}, TurnUsage{InputTokens: 1}},
		{TurnUsage{ProviderDurationMS: math.MaxInt64}, TurnUsage{ProviderDurationMS: 1}},
		{TurnUsage{Cost: math.MaxFloat64}, TurnUsage{Cost: math.MaxFloat64}},
		{TurnUsage{}, TurnUsage{OutputTokens: -1}},
	} {
		if _, err := addParticipationUsage(test.total, test.next); err == nil {
			t.Fatalf("invalid sum accepted: %#v", test)
		}
	}
}

func TestParticipationCanRequireUsageReporting(t *testing.T) {
	config := DefaultConversationCoordinatorConfig()
	config.RequireUsageReporting = true
	_, err := NewConversationCoordinator(NewConversationService(NewMemoryStore()), ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return nil, nil
	}), ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
		return ParticipationProposal{}, nil
	}), config)
	if err == nil {
		t.Fatal("unmetered provider accepted")
	}
}

func TestMeteredParticipationReservesBeforeCallsAndRejectsOverage(t *testing.T) {
	for _, scenario := range []string{"admitted", "insufficient", "overage"} {
		t.Run(scenario, func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "local", ID: "reserved"}
			channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Research", IdempotencyKey: "channel"})
			if err != nil {
				t.Fatal(err)
			}
			trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review evidence", "trigger")
			policy := &BudgetPolicy{MaxTurns: 3, MaxTotalTokens: 100}
			if scenario == "insufficient" {
				policy.MaxTotalTokens = 59
			}
			scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{Budget: policy})
			if err != nil {
				t.Fatal(err)
			}
			expectedLimit := policy.MaxTotalTokens
			policy.MaxTotalTokens = 1 // configuration must not retain caller-owned pointers
			scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, trigger.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scheduled.Run.Budget == nil || scheduled.Run.Budget.MaxTotalTokens != expectedLimit {
				t.Fatal("scheduler budget was not copied")
			}
			participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
				return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "a"}, SemanticRoles: []string{"reviewer"}}}, nil
			})
			calls := 0
			provider := MeteredParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (MeteredParticipationProposal, error) {
				calls++
				if input.Budget == nil || input.Budget.InputTokens != 20 || input.Budget.OutputTokens != 10 {
					t.Errorf("missing host allowance: %#v", input.Budget)
				}
				report := MeteredParticipationProposal{Usage: TurnUsage{InputTokens: 19, OutputTokens: 7}, Proposal: ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Bounded evidence review.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true}}}
				if scenario == "overage" {
					report.Usage.OutputTokens = 11
					input.Budget.OutputTokens = 999
				}
				return report, nil
			})
			config := DefaultConversationCoordinatorConfig()
			config.MaximumParticipants = 2
			config.MaximumConcurrency = 2
			config.ProposalBudget = ParticipationProposalBudget{InputTokens: 20, OutputTokens: 10}
			coordinator, err := NewConversationCoordinator(service, participants, provider, config)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: scheduled.Run.ID, WorkerID: "worker"}, runner)
			if scenario == "insufficient" {
				if !errors.Is(err, ErrBudgetExhausted) || result.Run.Status != AgentRunStatusPaused || calls != 0 {
					t.Fatalf("unadmitted call: %#v %v calls=%d", result, err, calls)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			if scenario == "overage" {
				if err == nil || result.Run.Status != AgentRunStatusFailed || result.Run.BudgetUsage.OutputTokens != 11 {
					t.Fatalf("overage accepted or uncharged: %#v %v", result, err)
				}
				messages, listErr := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: channel.ID})
				if listErr != nil || len(messages) != 1 {
					t.Fatalf("overage published: %#v %v", messages, listErr)
				}
				return
			}
			if err != nil || result.Run.Status != AgentRunStatusCompleted || result.Run.BudgetUsage.InputTokens != 19 || result.Run.BudgetUsage.OutputTokens != 7 {
				t.Fatalf("admitted result: %#v %v", result, err)
			}
			// Recovery of a persisted round needs no new provider reservation.
			planned, err := runner.PlanTurnBudget(t.Context(), TurnExecutionContext{Run: scheduled.Run})
			if err != nil || planned != (BudgetUsage{}) {
				t.Fatalf("replay reserved model capacity: %#v %v", planned, err)
			}
			replay, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
			if err != nil || replay.Usage != (TurnUsage{}) || calls != 1 {
				t.Fatalf("replay charged: %#v %v calls=%d", replay, err, calls)
			}
		})
	}
}

func TestMeteredParticipationRecoversCommittedChargeAfterRestartAndOptOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round-recovery.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := NewConversationService(store)
	scope := Scope{Kind: "local", ID: "recovery"}
	channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Research", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review evidence", "trigger")
	request := conversationAgentRunRequest(channel, trigger)
	request.Budget = &BudgetPolicy{MaxTurns: 3, MaxTotalTokens: 100}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "a"}, SemanticRoles: []string{"reviewer"}}}, nil
	})
	calls := 0
	provider := MeteredParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (MeteredParticipationProposal, error) {
		calls++
		return MeteredParticipationProposal{Usage: TurnUsage{InputTokens: 19, OutputTokens: 7}, Proposal: ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Evidence reviewed.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true}}}, nil
	})
	makeRunner := func() *ConversationRunTurnRunner {
		coordinator, err := NewConversationCoordinator(service, participants, provider, DefaultConversationCoordinatorConfig())
		if err != nil {
			t.Fatal(err)
		}
		runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{RequireParticipationOptIn: true})
		if err != nil {
			t.Fatal(err)
		}
		return runner
	}
	// The round commits, but its originating turn outcome is lost before saving.
	input := TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "original-turn"}}
	outcome, err := makeRunner().RunTurn(t.Context(), input)
	if err != nil || outcome.Usage.InputTokens != 19 {
		t.Fatalf("initial round: %#v %v", outcome, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service = NewConversationService(store)
	runner := makeRunner()
	for _, disable := range []bool{false, true} {
		if disable {
			current, err := service.GetConversation(t.Context(), scope, channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			enabled = false
			if _, err := service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: current.Revision, ParticipationEnabled: &enabled}); err != nil {
				t.Fatal(err)
			}
		}
		recovered, err := runner.RunTurn(t.Context(), input)
		if err != nil || recovered.Usage.InputTokens != 19 || recovered.Usage.OutputTokens != 7 || calls != 1 {
			t.Fatalf("recovery lost charge: %#v %v calls=%d", recovered, err, calls)
		}
	}
	input.Turn = &AgentTurn{ID: "later-turn"}
	later, err := runner.RunTurn(t.Context(), input)
	if err != nil || later.Usage != (TurnUsage{}) || calls != 1 {
		t.Fatalf("later continuation double charged: %#v %v calls=%d", later, err, calls)
	}
}
