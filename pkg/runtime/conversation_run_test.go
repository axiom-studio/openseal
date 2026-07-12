package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConversationRunSchedulerIsIdempotentAndReconcilesMissedMessages(t *testing.T) {
	store := NewMemoryStore(50)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "scheduler"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"},
		Title: "Engineering", IdempotencyKey: "engineering-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Is the canary healthy?", "message-one")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{ConversationPageSize: 1, MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	created, replayed, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, first.ID)
	if err != nil || replayed || created == nil || created.Run == nil {
		t.Fatalf("initial schedule = %#v, replayed=%v, err=%v", created, replayed, err)
	}
	if created.Event == nil || created.Run.Kind != RunKindConversation || created.Run.ConcurrencyKey != conversation.ID ||
		created.Run.Owner != conversation.Owner || created.Run.Context[conversationRunContextTriggerID] != first.ID {
		t.Fatalf("scheduled conversation Run = %#v", created)
	}
	replayedResult, replayed, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, first.ID)
	if err != nil || !replayed || replayedResult == nil || replayedResult.Run.ID != created.Run.ID || replayedResult.Event != nil {
		t.Fatalf("schedule replay = %#v, replayed=%v, err=%v", replayedResult, replayed, err)
	}

	current, err := service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := postConversationRunTestMessage(t, service, current, ConversationParticipantAgent, MessageIntentHandoff, "Please review the release notes.", "message-two")
	current, err = service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	postConversationRunTestMessage(t, service, current, ConversationParticipantService, MessageIntentSystem, "Channel metadata synchronized.", "message-system")

	reconciled, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Conversations != 1 || reconciled.Messages != 3 || reconciled.Scheduled != 1 || reconciled.Replayed != 1 || reconciled.Skipped != 1 {
		t.Fatalf("reconcile result = %#v", reconciled)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation, Owner: &conversation.Owner})
	if err != nil || len(runs) != 2 {
		t.Fatalf("conversation Runs = %#v, %v", runs, err)
	}
	triggerIDs := map[interface{}]bool{}
	for _, run := range runs {
		triggerIDs[run.Context[conversationRunContextTriggerID]] = true
	}
	if !triggerIDs[first.ID] || !triggerIDs[second.ID] {
		t.Fatalf("scheduled trigger IDs = %#v", triggerIDs)
	}
	again, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil || again.Messages != 0 || again.Scheduled != 0 || again.Replayed != 0 {
		t.Fatalf("second reconciliation = %#v, %v", again, err)
	}
}

func TestConversationRunSchedulerRecoversAfterSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conversation-runs.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "restart"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
		Title: "Operations", IdempotencyKey: "operations-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "What is failing?", "restart-one")
	current, err := service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := postConversationRunTestMessage(t, service, current, ConversationParticipantUser, MessageIntentUpdate, "The database is healthy.", "restart-two")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewConversationRunScheduler(reopened, reopened, ConversationRunSchedulerConfig{ConversationPageSize: 10, MessagePageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil || result.Scheduled != 2 || result.Messages != 2 {
		t.Fatalf("restart reconciliation = %#v, %v", result, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	verified, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	runs, err := verified.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
	if err != nil || len(runs) != 2 {
		t.Fatalf("persisted conversation Runs = %#v, %v", runs, err)
	}
	triggerIDs := map[interface{}]bool{runs[0].Context[conversationRunContextTriggerID]: true, runs[1].Context[conversationRunContextTriggerID]: true}
	if !triggerIDs[first.ID] || !triggerIDs[second.ID] {
		t.Fatalf("persisted trigger IDs = %#v", triggerIDs)
	}
	cursor, err := NewConversationService(verified).GetCursor(ctx, scope, conversation.ID, ConversationParticipant{
		Type: ConversationParticipantService, ID: conversationRunSchedulerParticipant,
	})
	if err != nil || cursor == nil || cursor.ReadSequence != 2 || cursor.DeliveredSequence != 2 {
		t.Fatalf("persisted scheduler cursor = %#v, %v", cursor, err)
	}
}

func TestConversationRunTurnRunnerCompletesAndReplaysCommittedRound(t *testing.T) {
	store := NewMemoryStore(50)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "runner"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"},
		Title: "Engineering", IdempotencyKey: "runner-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Is the canary healthy?", "runner-trigger")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := conversationRunTestCoordinator(t, service)
	current, err := service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := coordinator.Coordinate(ctx, ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision, TriggerMessageID: trigger.ID,
		Policy: DefaultConversationArbitrationPolicy(), IdempotencyKey: conversationRunRoundKey(scope, conversation.ID, trigger.ID),
	})
	if err != nil || len(committed.Messages) != 1 {
		t.Fatalf("precommitted round = %#v, %v", committed, err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, Kind: RunKindConversation, WorkerID: "conversation-worker", Now: time.Now(),
		LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForConcurrencyKey: 1,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claimed conversation Run = %#v, %v", claimed, err)
	}
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := NewTurnCoordinator(store, store, store).Advance(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: claimed.ID, WorkerID: claimed.LeaseOwner, LeaseDuration: time.Minute,
		DefinitionID: binding.DefinitionID, DefinitionVersion: binding.DefinitionVersion,
		ModelProvider: binding.ModelProvider, Model: binding.Model, InputContextRefs: binding.InputContextRefs,
	}, binding.Runner)
	if err != nil || advanced == nil || advanced.Run.Status != AgentRunStatusCompleted ||
		advanced.Run.Output["participationRoundId"] != committed.Round.ID || advanced.Run.Output["replayed"] != true {
		t.Fatalf("advanced conversation Run = %#v, %v", advanced, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 {
		t.Fatalf("round replay duplicated messages: %#v, %v", messages, err)
	}
}

func TestConversationRunTurnRunnerExecutesAgentOwnedChannelThroughBoundAgent(t *testing.T) {
	store := NewMemoryStore(50)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "agent-channel"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"},
		Title: "Release assistant", IdempotencyKey: "release-assistant-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Summarize the release evidence.", "agent-trigger")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	if scheduled.Run.Owner != conversation.Owner || scheduled.Run.AssignedAgentID != conversation.Owner.ID {
		t.Fatalf("Agent conversation Run identity = %#v", scheduled.Run)
	}
	resolverCalls := 0
	agentTurns := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		resolverCalls++
		if run.Kind != RunKindAgentWork || run.AssignedAgentID != "agent-42" {
			t.Fatalf("hosted Agent projection = %#v", run)
		}
		return &TurnRunnerBinding{
			DefinitionID: "agent-definition", DefinitionVersion: "7", ModelProvider: "host", Model: "agent-model",
			InputContextRefs: []string{"skill:summarize@1"}, BudgetReservation: BudgetUsage{Turns: 1},
			Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				if input.Run.Kind != RunKindAgentWork || input.Run.AssignedAgentID != run.AssignedAgentID ||
					!strings.Contains(input.Run.Goal, "Summarize the release evidence") {
					t.Fatalf("bound Agent runner input = %#v", input.Run)
				}
				return &TurnOutcome{
					NextRunStatus: AgentRunStatusCompleted,
					OutputSummary: "Release evidence summarized",
					RunOutput:     map[string]interface{}{"summary": "The release evidence is healthy."},
					SkillSelections: []HostedSkillSelection{{
						SkillRef: "skill:summarize@1", Disposition: HostedSkillApplied, Summary: "Applied summarization",
					}},
				}, nil
			}),
		}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
	if err != nil || binding.DefinitionID != "agent-definition" || binding.DefinitionVersion != "7" ||
		binding.ModelProvider != "host" || binding.Model != "agent-model" || len(binding.InputContextRefs) != 1 ||
		binding.BudgetReservation.Turns != 1 {
		t.Fatalf("Agent conversation binding = %#v, %v", binding, err)
	}
	outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted ||
		outcome.RunOutput["messageId"] == "" || outcome.RunOutput["replayed"] != false || len(outcome.SkillSelections) != 1 {
		t.Fatalf("Agent conversation outcome = %#v, %v", outcome, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[1].Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-42"}) ||
		messages[1].Content != "The release evidence is healthy." || messages[1].ReplyToMessageID != trigger.ID ||
		len(messages[1].References) != 1 || messages[1].References[0].Kind != ConversationReferenceRun || messages[1].References[0].ID != scheduled.Run.ID {
		t.Fatalf("Agent channel messages = %#v, %v", messages, err)
	}
	replayed, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || replayed.RunOutput["replayed"] != true || resolverCalls != 1 {
		t.Fatalf("Agent response replay = %#v, calls=%d, err=%v", replayed, resolverCalls, err)
	}
	messages, err = service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 {
		t.Fatalf("Agent response replay duplicated messages: %#v, %v", messages, err)
	}
}

func TestConversationRunReconciliationDoesNotReplayLegacyCoordinatedMessages(t *testing.T) {
	store := NewMemoryStore(20)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "legacy-round"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"},
		Title: "Engineering", IdempotencyKey: "legacy-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Is the release ready?", "legacy-trigger")
	coordinator := conversationRunTestCoordinator(t, service)
	current, err := service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Coordinate(ctx, ConversationCoordinationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision, TriggerMessageID: trigger.ID,
		Policy: DefaultConversationArbitrationPolicy(), IdempotencyKey: "legacy-manual-round",
	}); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil || result.Messages != 2 || result.Scheduled != 0 || result.Skipped != 2 {
		t.Fatalf("legacy reconciliation = %#v, %v", result, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
	if err != nil || len(runs) != 0 {
		t.Fatalf("legacy messages were replayed as Runs: %#v, %v", runs, err)
	}
}

func TestConversationRunTurnRunnerRetriesWithoutPersistingProviderErrors(t *testing.T) {
	store := NewMemoryStore(20)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "retry"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
		Title: "Operations", IdempotencyKey: "retry-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "What changed?", "retry-trigger")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}}}, nil
	})
	proposals := ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
		return ParticipationProposal{}, errors.New("provider unavailable; credential=sk-never-persist")
	})
	coordinator, err := NewConversationCoordinator(service, participants, proposals, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{
		MaximumRetries: 2, InitialRetryDelay: time.Second, MaximumRetryDelay: 2 * time.Second,
		Policy: DefaultConversationArbitrationPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.now = func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) }
	outcome, err := runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusSleeping || outcome.WakeCondition == nil ||
		outcome.ContinuationCheckpoint["lastRetryReason"] != "participant_runtime_unavailable" {
		t.Fatalf("retry outcome = %#v, %v", outcome, err)
	}
	encoded := outcome.OutputSummary + outcome.ContinuationCheckpoint["lastRetryReason"].(string)
	if strings.Contains(encoded, "sk-never-persist") || strings.Contains(encoded, "provider unavailable") {
		t.Fatalf("provider error leaked into durable retry state: %q", encoded)
	}
}

func conversationRunTestCoordinator(t *testing.T, service *ConversationService) *ConversationCoordinator {
	t.Helper()
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{
			Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"},
		}}, nil
	})
	proposals := ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
		return ParticipationProposal{
			WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Telemetry confirms all three replicas passed the smoke checks.",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			Signals:  ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true},
		}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, proposals, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func postConversationRunTestMessage(
	t *testing.T,
	service *ConversationService,
	conversation *Conversation,
	senderType ConversationParticipantType,
	intent ConversationMessageIntent,
	content string,
	key string,
) *ChannelMessage {
	t.Helper()
	result, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: senderType, ID: "sender-" + string(senderType)},
		Intent: intent, Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

func conversationRunRoundKey(scope Scope, conversationID, triggerID string) string {
	return "participation-round:" + hashString(scope.Kind+"\x00"+scope.ID+"\x00"+conversationID+"\x00"+triggerID)
}
