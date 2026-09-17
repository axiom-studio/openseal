package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type participationTestStore interface {
	ConversationStore
	RunCommandStore
}

func TestParticipationOptInStartsAtNewMessagesAndSurvivesRestart(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store participationTestStore = NewMemoryStore()
			path := filepath.Join(t.TempDir(), "participation.db")
			if backend == "sqlite" {
				db, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				store = db
			}
			service := NewConversationService(store)
			scope := Scope{Kind: "local", ID: "default"}
			channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, Title: "Evidence", IdempotencyKey: "channel"})
			if err != nil {
				t.Fatal(err)
			}
			current := func() *Conversation {
				c, err := service.GetConversation(t.Context(), scope, channel.ID)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			post := func(key string) *ChannelMessage {
				return postConversationRunTestMessage(t, service, current(), ConversationParticipantUser, MessageIntentQuestion, "Review "+key, key)
			}
			enable := func(value bool) *Conversation {
				c, err := service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: current().Revision, ParticipationEnabled: &value})
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{RequireParticipationOptIn: true, MessagePageSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			old := post("before-opt-in")
			if got, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, old.ID); err != nil || got != nil {
				t.Fatalf("unconfigured scheduled: %#v %v", got, err)
			}
			enabled := enable(true)
			if enabled.Participation == nil || !enabled.Participation.Enabled || enabled.Participation.AfterSequence != 1 {
				t.Fatalf("activation boundary: %#v", enabled.Participation)
			}
			enabled.Participation.AfterSequence = 99
			if current().Participation.AfterSequence != 1 {
				t.Fatal("read result mutated saved participation")
			}
			second := post("enabled")
			repeated := enable(true)
			if repeated.Participation.AfterSequence != 1 {
				t.Fatal("repeated enable discarded pending messages")
			}
			queued, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, second.ID)
			if err != nil || queued == nil {
				t.Fatalf("enabled did not schedule: %v", err)
			}
			enable(false)
			third := post("while-disabled")
			if got, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, third.ID); err != nil || got != nil {
				t.Fatalf("disabled scheduled: %#v %v", got, err)
			}
			enable(true)
			if current().Participation.AfterSequence != 3 {
				t.Fatal("reactivation did not skip old messages")
			}
			fourth := post("after-reactivation")
			if backend == "sqlite" {
				if err := store.(*SQLiteStore).Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				store = reopened
				service = NewConversationService(store)
				scheduler, err = NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{RequireParticipationOptIn: true, MessagePageSize: 1})
				if err != nil {
					t.Fatal(err)
				}
			}
			if current().Participation.AfterSequence != 3 || !current().Participation.Enabled {
				t.Fatal("participation not durable")
			}
			result, err := scheduler.ReconcileScope(t.Context(), scope)
			if err != nil || result.Scheduled != 1 {
				t.Fatalf("reconciliation: %#v %v", result, err)
			}
			if got, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, old.ID); err != nil || got != nil {
				t.Fatal("historical message woke after enable")
			}
			run, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, fourth.ID)
			if err != nil || run == nil {
				t.Fatal(err)
			}
			runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{RequireParticipationOptIn: true})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: queued.Run})
			if err != nil || outcome.RunOutput["participationSkipped"] != true {
				t.Fatalf("old queued run resumed after re-enable: %#v %v", outcome, err)
			}
			before := current()
			disable := false
			if _, err := service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: before.Revision - 1, ParticipationEnabled: &disable}); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("stale opt-out: %v", err)
			}
			outcome, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: run.Run})
			if err != nil || outcome.NextRunStatus != AgentRunStatusCompleted || outcome.RunOutput["participationRoundId"] == nil {
				t.Fatalf("fresh round blocked: %#v %v", outcome, err)
			}
		})
	}
}

func TestParticipationOptOutBlocksQueuedAndInFlightPublication(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "local", ID: "default"}
	channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, Title: "Evidence", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review this evidence", "question")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{RequireParticipationOptIn: true})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "analyst"}, SemanticRoles: []string{"reviewer"}}, {Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "second"}, SemanticRoles: []string{"reviewer"}}}, nil
	})
	provider := ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Late model answer", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true}}, nil
	})
	config := DefaultConversationCoordinatorConfig()
	config.MaximumConcurrency = 1
	coordinator, err := NewConversationCoordinator(service, participants, provider, config)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{RequireParticipationOptIn: true})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start")
	}
	current, err := service.GetConversation(t.Context(), scope, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	enabled = false
	if _, err := service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: current.Revision, ParticipationEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not finish")
	}
	messages, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: channel.ID})
	if err != nil || len(messages) != 1 {
		t.Fatalf("late publication: %#v %v", messages, err)
	}
	binding, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	result, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
	if err != nil || result.RunOutput["participationSkipped"] != true || calls.Load() != 1 {
		t.Fatalf("queued work reached model after opt-out: %#v %v calls=%d", result, err, calls.Load())
	}
}

func TestParticipationArchiveRequiresFreshOptIn(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "local", ID: "default"}
	channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, Title: "Research", IdempotencyKey: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review evidence", "before-archive")
	for _, status := range []ConversationStatus{ConversationStatusArchived, ConversationStatusActive} {
		channel, err = service.GetConversation(t.Context(), scope, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, Status: &status})
		if err != nil {
			t.Fatal(err)
		}
		if channel.Participation.Enabled {
			t.Fatal("archive/restore re-enabled participation")
		}
	}
	channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if conversationParticipationAllows(channel, trigger) {
		t.Fatal("restored channel woke pre-archive message")
	}
}

func TestParticipationOptOutDiscardsAgentOutputAndKeepsUsage(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusRunning} {
		t.Run(string(status), func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "local", ID: "default"}
			channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, Title: "Research", IdempotencyKey: "agent"})
			if err != nil {
				t.Fatal(err)
			}
			enabled := true
			channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
			if err != nil {
				t.Fatal(err)
			}
			trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review evidence", "trigger")
			scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{RequireParticipationOptIn: true})
			if err != nil {
				t.Fatal(err)
			}
			scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, trigger.ID)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(ctx context.Context, _ TurnExecutionContext) (*TurnOutcome, error) {
					calls++
					current, err := service.GetConversation(ctx, scope, channel.ID)
					if err != nil {
						return nil, err
					}
					enabled = false
					if _, err := service.UpdateConversation(ctx, UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: current.Revision, ParticipationEnabled: &enabled}); err != nil {
						return nil, err
					}
					return &TurnOutcome{NextRunStatus: status, Model: "synthetic", Usage: TurnUsage{InputTokens: 12, OutputTokens: 8}, RunOutput: map[string]interface{}{"reply": "Late answer"}, ProposedRunbook: &TurnRunbookProposal{Entrypoint: "late-operation"}}, nil
				})}, nil
			})
			runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{RequireParticipationOptIn: true, AgentTurns: agentTurns})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
			if err != nil {
				t.Fatal(err)
			}
			if outcome.RunOutput["participationSkipped"] != true || outcome.Usage.InputTokens != 12 || outcome.Usage.OutputTokens != 8 || outcome.ProposedRunbook != nil || calls != 1 {
				t.Fatalf("late outcome: %#v calls=%d", outcome, calls)
			}
			messages, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: channel.ID})
			if err != nil || len(messages) != 1 {
				t.Fatalf("late reply: %#v %v", messages, err)
			}
		})
	}
}

func TestConversationRunPinsResolvedPolicyAcrossRoundRecovery(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "local", ID: "policy"}
	channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Research", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review evidence", "trigger")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{ResolvePolicy: func(context.Context, *Conversation) (ConversationArbitrationPolicy, error) {
		calls++
		if calls > 1 {
			return ConversationArbitrationPolicy{}, errors.New("team changed after round committed")
		}
		policy := DefaultConversationArbitrationPolicy()
		policy.MaximumSpeakers = 1
		return policy, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
		if err != nil || outcome.NextRunStatus != AgentRunStatusCompleted {
			t.Fatalf("round recovery: %#v %v", outcome, err)
		}
	}
	rounds, err := service.ListParticipationRounds(t.Context(), ParticipationRoundFilter{Scope: scope, ConversationID: channel.ID})
	if err != nil || len(rounds) != 1 || rounds[0].Round.Policy.MaximumSpeakers != 1 || calls != 1 {
		t.Fatalf("resolved policy lost on recovery: %#v %v calls=%d", rounds, err, calls)
	}
}
