package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
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

func TestConversationRunTurnRunnerArbitratesOneGovernedTeamActionAndCompletesTruthfully(t *testing.T) {
	store := NewMemoryStore(50)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "team-action"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"},
		Title: "Engineering", IdempotencyKey: "team-action-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Create the release objective.", "team-action-trigger")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"}, Priority: 10},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-2"}, SemanticRoles: []string{"reviewer"}},
		}, nil
	})
	proposals := ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		if input.Participant.ID == "agent-2" {
			return ParticipationProposal{}, nil
		}
		return ParticipationProposal{
			WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "I propose creating the release objective through the governed approval gate.",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			Signals:  ParticipationSignals{DirectlyMentioned: true, HasNewInformation: true, RoleRelevant: true},
			ProposedAction: &TurnAction{
				Type: "skill_action", Capability: "openseal.objectives.create", Summary: "Create the release objective", InputRef: "/call",
			},
			ActionInputs: map[string]interface{}{"call": map[string]interface{}{"title": "Release", "goal": "Ship safely"}},
		}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, proposals, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	teamActions := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if run.Owner != conversation.Owner || run.Kind != RunKindConversation {
			t.Fatalf("Team action binding run = %#v", run)
		}
		return &TurnRunnerBinding{
			DeploymentID: "team:engineering",
			ModelActions: []capability.ModelAction{{
				Name: "openseal.objectives.create", SkillID: "openseal.objectives", Version: "1.0.0", Action: "create",
				BindingID: "bundled:objectives", BindingRevision: 1,
			}},
		}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{TeamActions: teamActions})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
	if err != nil || binding.DeploymentID != "team:engineering" || len(binding.ModelActions) != 1 {
		t.Fatalf("Team action binding = %#v, %v", binding, err)
	}
	first, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || first.NextRunStatus != AgentRunStatusRunning || len(first.ProposedActions) != 1 ||
		first.ProposedActions[0].Capability != "openseal.objectives.create" || first.ProposedActions[0].IdempotencyKey == "" {
		t.Fatalf("Team action proposal = %#v, %v", first, err)
	}
	arguments, err := resolveTurnActionInput(first.ContinuationCheckpoint, first.ProposedActions[0].InputRef)
	if err != nil || arguments["title"] != "Release" {
		t.Fatalf("Team action arguments = %#v, %v", arguments, err)
	}
	if first.ContinuationCheckpoint[teamActionAssignedAgentCheckpointKey] != "agent-1" {
		t.Fatalf("Team action roster attribution = %#v", first.ContinuationCheckpoint)
	}
	resumed := cloneAgentRun(scheduled.Run)
	resumed.Checkpoint = cloneMap(first.ContinuationCheckpoint)
	resumed.Checkpoint["lastAction"] = map[string]interface{}{
		"status": "succeeded", "skillId": "openseal.objectives", "action": "create",
		"bindingId": "bundled:objectives", "bindingRevision": float64(1),
		"result": map[string]interface{}{
			"resourceType": "objective", "operation": "create", "created": true,
			"objective": map[string]interface{}{"id": "objective-release", "title": "Release", "status": "draft", "revision": float64(1)},
		},
	}
	second, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: resumed})
	if err != nil || second.NextRunStatus != AgentRunStatusCompleted || len(second.ProposedActions) != 0 {
		t.Fatalf("Team action completion = %#v, %v", second, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 3 || messages[1].Sender.ID != "agent-1" ||
		len(messages[1].References) != 1 || messages[1].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}) ||
		messages[2].Content != "Objective “Release” was created successfully and is now draft." ||
		len(messages[2].References) != 2 ||
		messages[2].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}) ||
		messages[2].References[1] != (ConversationReference{Kind: ConversationReferenceObjective, ID: "objective-release", Version: 1}) ||
		messages[2].ResolvesMessageID != trigger.ID {
		t.Fatalf("Team action channel messages = %#v, %v", messages, err)
	}
	replay, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: resumed})
	if err != nil || replay.RunOutput["replayed"] != true {
		t.Fatalf("Team action completion replay = %#v, %v", replay, err)
	}
	messages, _ = service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if len(messages) != 3 {
		t.Fatalf("Team action replay duplicated messages: %#v", messages)
	}
}

func mustConversationRunScheduler(t *testing.T, store *MemoryStore) *ConversationRunScheduler {
	t.Helper()
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
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
			DeploymentID: "agent-42", DefinitionID: "agent-definition", DefinitionVersion: "7", ModelProvider: "host", Model: "agent-model",
			ModelActions:     []capability.ModelAction{{Name: "openseal.objectives.create", SkillID: "openseal.objectives", Version: "1.0.0", Action: "create", BindingID: "bundled:objectives", BindingRevision: 1}},
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
	if err != nil || binding.DeploymentID != "agent-42" || binding.DefinitionID != "agent-definition" || binding.DefinitionVersion != "7" ||
		binding.ModelProvider != "host" || binding.Model != "agent-model" || len(binding.InputContextRefs) != 1 ||
		binding.BudgetReservation.Turns != 1 || len(binding.ModelActions) != 1 || binding.ModelActions[0].BindingID != "bundled:objectives" {
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
	cursor, err := service.GetCursor(ctx, scope, conversation.ID, ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-42"})
	if err != nil || cursor == nil || cursor.DeliveredSequence != trigger.Sequence || cursor.ReadSequence != trigger.Sequence {
		t.Fatalf("Agent channel read cursor = %#v, %v", cursor, err)
	}
	replayed, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || replayed.RunOutput["replayed"] != true || resolverCalls != 1 {
		t.Fatalf("Agent response replay = %#v, calls=%d, err=%v", replayed, resolverCalls, err)
	}
	messages, err = service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 {
		t.Fatalf("Agent response replay duplicated messages: %#v, %v", messages, err)
	}
	cursor, err = service.GetCursor(ctx, scope, conversation.ID, ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-42"})
	if err != nil || cursor == nil || cursor.DeliveredSequence != messages[1].Sequence || cursor.ReadSequence != messages[1].Sequence {
		t.Fatalf("replayed Agent channel read cursor = %#v, %v", cursor, err)
	}
	reconciled, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil || reconciled.Scheduled != 0 || reconciled.Replayed != 1 || reconciled.Skipped != 1 {
		t.Fatalf("Agent response reconciliation = %#v, %v", reconciled, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation, Owner: &conversation.Owner})
	if err != nil || len(runs) != 1 || runs[0].ID != scheduled.Run.ID {
		t.Fatalf("Agent reply scheduled a response loop: %#v, %v", runs, err)
	}
}

func TestConversationRunTurnRunnerProjectsGovernedAgentResultWithoutModelRenarration(t *testing.T) {
	store := NewMemoryStore(50)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "agent-result"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"},
		Title: "Release assistant", IdempotencyKey: "agent-result-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Create the release objective after approval.", "agent-result-trigger")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DeploymentID: "agent-42", DefinitionID: "agent-definition", DefinitionVersion: "1",
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				modelCalls++
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "It is still awaiting approval."}}, nil
			}),
		}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	resumed := cloneAgentRun(scheduled.Run)
	resumed.Checkpoint = map[string]interface{}{
		"lastAction": map[string]interface{}{
			"status": ActionCallStatusSucceeded,
			"result": map[string]interface{}{
				"resourceType": "objective", "operation": "create", "created": true,
				"objective": &Objective{ID: "objective-release", Title: "Release readiness", Status: ObjectiveStatusDraft, Revision: 1},
			},
		},
	}
	outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: resumed})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted || modelCalls != 0 ||
		outcome.RunOutput["resourceType"] != "objective" || outcome.RunOutput["resourceId"] != "objective-release" {
		t.Fatalf("governed Agent completion = %#v, modelCalls=%d, err=%v", outcome, modelCalls, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[1].Content != "Objective “Release readiness” was created successfully and is now draft." ||
		strings.Contains(messages[1].Content, "awaiting approval") || len(messages[1].References) != 2 ||
		messages[1].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}) ||
		messages[1].References[1] != (ConversationReference{Kind: ConversationReferenceObjective, ID: "objective-release", Version: 1}) {
		t.Fatalf("governed Agent message = %#v, %v", messages, err)
	}
	replayed, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: resumed})
	if err != nil || replayed.RunOutput["replayed"] != true || modelCalls != 0 {
		t.Fatalf("governed Agent replay = %#v, modelCalls=%d, err=%v", replayed, modelCalls, err)
	}
	messages, _ = service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if len(messages) != 2 {
		t.Fatalf("governed Agent replay duplicated messages: %#v", messages)
	}
}

func TestGovernedConversationActionCompletionProjectsInitiativeIdentity(t *testing.T) {
	run := &AgentRun{
		ID: "run-initiative", Checkpoint: map[string]interface{}{
			"lastAction": map[string]interface{}{
				"status": "succeeded",
				"result": map[string]interface{}{
					"resourceType": "initiative", "operation": "update", "created": false,
					"initiative": map[string]interface{}{
						"id": "initiative-research", "title": "Customer research", "status": "active", "revision": float64(4),
					},
				},
			},
		},
	}
	completion, ok := governedConversationActionCompletion(run)
	if !ok || completion.Content != "Initiative “Customer research” was updated successfully and is now active." ||
		completion.ResourceType != "initiative" || completion.ResourceID != "initiative-research" ||
		len(completion.References) != 2 ||
		completion.References[1] != (ConversationReference{Kind: ConversationReferenceInitiative, ID: "initiative-research", Version: 4}) {
		t.Fatalf("initiative completion = %#v, ok=%v", completion, ok)
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
