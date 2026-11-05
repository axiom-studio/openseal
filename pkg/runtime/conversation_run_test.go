package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestAgentConversationGoalSeparatesCurrentRequestFromOldCommands(t *testing.T) {
	conversation := &Conversation{ID: "chat", Title: "Assistant"}
	previous := &ChannelMessage{ID: "old", Sequence: 1, Content: "Create a PDF"}
	trigger := &ChannelMessage{ID: "new", Sequence: 2, Content: "Reply with exactly OK"}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, trigger, []*ChannelMessage{previous, trigger}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(goal, "Respond only to currentMessage") {
		t.Fatal("prompt does not prioritize the triggering request")
	}
	var payload struct {
		Messages       []agentConversationPromptMessage `json:"messages"`
		CurrentMessage agentConversationPromptMessage   `json:"currentMessage"`
	}
	_, raw, ok := strings.Cut(goal, "\n\n")
	if !ok || json.Unmarshal([]byte(raw), &payload) != nil || len(payload.Messages) != 1 ||
		payload.Messages[0].ID != previous.ID || payload.CurrentMessage.ID != trigger.ID || payload.CurrentMessage.Content != trigger.Content {
		t.Fatalf("current request was mixed with history: %s", goal)
	}
}

func TestConversationRunSchedulerIsIdempotentAndReconcilesMissedMessages(t *testing.T) {
	store := NewMemoryStore()
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
	// A member's message without an @mention is not a wake in a Team channel;
	// a second human message is.
	second := postConversationRunTestMessage(t, service, current, ConversationParticipantUser, MessageIntentQuestion, "Please review the release notes.", "message-two")
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

func TestConversationMessageStartsRunSkipsApprovalCoordinatorProjections(t *testing.T) {
	conversation := &Conversation{
		ID: "approval-channel", Scope: Scope{Kind: "tenant", ID: "11"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coding-agent"}, Status: ConversationStatusActive,
	}
	message := &ChannelMessage{
		ID: "approval-request", Scope: conversation.Scope, ConversationID: conversation.ID,
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "approval-coordinator"},
		Intent: MessageIntentApprovalRequest,
	}
	if conversationMessageStartsRun(conversation, message) {
		t.Fatal("approval coordinator request recursively started an Agent Run")
	}
	message.Intent = MessageIntentUpdate
	if conversationMessageStartsRun(conversation, message) {
		t.Fatal("approval coordinator outcome recursively started an Agent Run")
	}
	message.Sender.ID = "release-coordinator"
	if !conversationMessageStartsRun(conversation, message) {
		t.Fatal("unrelated service handoff was suppressed")
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

func TestConversationRunSchedulerProjectsCanceledRequestExactlyOnce(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "canceled-conversation"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
		Title: "Operations", IdempotencyKey: "canceled-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"},
		Intent: MessageIntentQuestion, Content: "Pause the project.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "canceled-trigger",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := mustConversationRunScheduler(t, store)
	scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, posted.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := NewRunCommandService(store).CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: scheduled.Run.ID, ExpectedRevision: scheduled.Run.Revision, Kind: AgentRunCommandCancel,
		Actor: ActivityActor{Type: "user", ID: "user-1"}, Summary: "Cancel the proposal",
	})
	if err != nil || canceled.Run.Status != AgentRunStatusCanceled {
		t.Fatalf("canceled conversation Run = %#v, %v", canceled, err)
	}
	result, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil || result.Results != 1 {
		t.Fatalf("canceled result reconciliation = %#v, %v", result, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 {
		t.Fatalf("canceled channel messages = %#v, %v", messages, err)
	}
	message := messages[1]
	if message.Sender != (ConversationParticipant{Type: ConversationParticipantService, ID: "openseal.conversation"}) ||
		message.Intent != MessageIntentSystem || message.Content != "This request was canceled before completion." ||
		message.ReplyToMessageID != posted.Message.ID || message.ResolvesMessageID != posted.Message.ID ||
		len(message.References) != 1 || message.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}) {
		t.Fatalf("canceled channel result = %#v", message)
	}
	replay, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil || replay.Results != 0 {
		t.Fatalf("canceled result replay = %#v, %v", replay, err)
	}
	messages, _ = service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if len(messages) != 2 {
		t.Fatalf("canceled result duplicated: %#v", messages)
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
	store := NewMemoryStore()
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
			ModelActions:      []capability.ModelAction{{Name: "openseal.objectives.create", SkillID: "openseal.objectives", Version: "1.0.0", Action: "create", BindingID: "bundled:objectives", BindingRevision: 1}},
			RunbookOperations: []HostedRunbookOperation{{Entrypoint: "release-now", Name: "Release now", Description: "Run the release operation"}},
			InputContextRefs:  []string{"skill:summarize@1"}, BudgetReservation: BudgetUsage{Turns: 1},
			Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				if input.Run.Kind != RunKindAgentWork || input.Run.AssignedAgentID != run.AssignedAgentID ||
					!strings.Contains(input.Run.Goal, "Summarize the release evidence") ||
					!strings.Contains(input.Run.Goal, `"entrypoint":"release-now"`) {
					t.Fatalf("bound Agent runner input = %#v", input.Run)
				}
				return &TurnOutcome{
					NextRunStatus: AgentRunStatusCompleted,
					OutputSummary: "Release evidence summarized",
					RunOutput: map[string]interface{}{
						"summary": "The release evidence is healthy.", "broadcastToChannel": true,
					},
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
		!messages[1].BroadcastToChannel ||
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

func TestAgentConversationRejectsUnverifiedApprovalClaims(t *testing.T) {
	for _, claim := range []string{
		"The approval has been submitted.",
		"The permission request was already submitted and is pending approval.",
		"We are awaiting approval.",
	} {
		if !unverifiedApprovalClaim(claim) {
			t.Fatalf("unverified approval claim was accepted: %q", claim)
		}
	}
	for _, truthful := range []string{
		"I need approval before I can do that.",
		"Please ask an administrator to configure the governed capability.",
		"No request has been created.",
	} {
		if unverifiedApprovalClaim(truthful) {
			t.Fatalf("truthful capability guidance was rejected: %q", truthful)
		}
	}

	conversation := &Conversation{ID: "channel-1", Title: "Operations"}
	trigger := &ChannelMessage{ID: "message-1"}
	history := []*ChannelMessage{{
		ID: "old-answer", Sequence: 1,
		Sender:  ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-42"},
		Intent:  MessageIntentAnswer,
		Content: "The prior engage-now operation completed successfully.",
	}}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, trigger, history, []HostedRunbookOperation{{
		Entrypoint: "engage-now", Name: "Engage now", Description: "Run the reviewed engagement operation",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(goal, "Never state or imply that an approval") ||
		!strings.Contains(goal, "directly callable through proposedRunbook") ||
		!strings.Contains(goal, "configured identity or persona is behavior, not authority") ||
		!strings.Contains(goal, "must never veto an authorized user's request to reconfigure this Agent") ||
		!strings.Contains(goal, "Do not answer a configuration command in character") ||
		!strings.Contains(goal, `"activeRuns":[]`) ||
		!strings.Contains(goal, "Historical Messages are conversational context, not current Run state") ||
		!strings.Contains(goal, "historical completed, failed, or canceled Run never prevents a new invocation") ||
		!strings.Contains(goal, `"entrypoint":"engage-now"`) {
		t.Fatalf("Agent conversation truthfulness contract missing from goal: %s", goal)
	}
}

func TestAgentConversationGoalProjectsActiveWorkFromTheSameChannel(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "active-channel"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Agent work", IdempotencyKey: "active-agent-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	portfolio := NewPortfolioService(store)
	root, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Respond to an Agent channel message", Source: RunSourceChat,
		Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindAgentWork, ParentRunID: root.ID, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Run the on-demand engagement operation", Source: RunSourceFork,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	goal, err := runner.agentConversationGoal(ctx, conversation, &ChannelMessage{
		ID: "trigger", Scope: scope, ConversationID: conversation.ID,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(goal, `"activeRuns":[`) || !strings.Contains(goal, `"id":"`+child.ID+`"`) ||
		!strings.Contains(goal, `"goal":"Run the on-demand engagement operation"`) ||
		!strings.Contains(goal, "Only if matching work appears in ActiveRuns") {
		t.Fatalf("active Run truth was not projected into Agent conversation goal: %s", goal)
	}
}

// Operation invocation is the model's decision through its runbook operation
// tools. Ordinary wording such as "run it" must never start the Agent's sole
// operation without a model turn.
func TestAgentConversationLeavesOperationInvocationToTheModel(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "repeatable-operation"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Community engagement", Goal: "Engage with relevant communities", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: "reddit-operations", DefinitionVersion: "1", TriggerID: "scheduled",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "scheduled-engage", Schedule: &runbook.Schedule{
			Cron: "0 0 * * * *", Timezone: "UTC",
		}},
		Status: RunbookActivationActive, IdempotencyKey: "scheduled-engagement",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: objective.Title,
		Origin:         &ConversationReference{Kind: ConversationReferenceObjective, ID: objective.ID, Version: objective.Revision},
		IdempotencyKey: "repeatable-operation-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	postConversationRunTestMessage(t, service, conversation, ConversationParticipantAgent, MessageIntentAnswer,
		"The on-demand engagement operation already ran to completion in run historical-1.", "historical-completion")
	conversation, err = service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion,
		"Use my card ending with 1001 to buy it, don't run it in a background task", "repeat-operation")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	operation := HostedRunbookOperation{
		Entrypoint: "ondemand-engage", Name: "On-demand engagement",
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false},
	}
	scheduledOperation := HostedRunbookOperation{
		Entrypoint: "scheduled-engage", Name: "Scheduled engagement",
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false},
	}
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DeploymentID: owner.ID, DefinitionID: "reddit-agent", DefinitionVersion: "1",
			RunbookOperations: []HostedRunbookOperation{operation, scheduledOperation},
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				modelCalls++
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Buying it now", RunOutput: map[string]interface{}{"summary": "Buying it now"}}, nil
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
	outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || outcome == nil || outcome.ProposedRunbook != nil || modelCalls != 1 {
		t.Fatalf("operation was started without the model: %#v, modelCalls=%d, err=%v", outcome, modelCalls, err)
	}
}

func TestConversationRunTurnRunnerProjectsGovernedAgentResultWithoutModelRenarration(t *testing.T) {
	store := NewMemoryStore()
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

func TestConversationRunTurnRunnerAttachesActionArtifactsToSourceReply(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "artifact-reply"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"},
		Title: "Chart assistant", IdempotencyKey: "artifact-reply-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Draw the chart.", "artifact-reply-trigger")
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
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Your chart is ready."}}, nil
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
	resumed.Checkpoint = map[string]interface{}{"lastAction": map[string]interface{}{
		"status": ActionCallStatusSucceeded,
		"result": map[string]interface{}{"state": "ready", "artifactRefs": []interface{}{
			map[string]interface{}{"id": "market-xirr", "version": float64(1), "requirementName": "chart"},
		}},
	}}
	outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: resumed})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted || modelCalls != 1 {
		t.Fatalf("artifact response = %#v, modelCalls=%d, err=%v", outcome, modelCalls, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[1].Content != "Your chart is ready." || len(messages[1].References) != 2 ||
		messages[1].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}) ||
		messages[1].References[1] != (ConversationReference{Kind: ConversationReferenceArtifact, ID: "market-xirr", Version: 1}) {
		t.Fatalf("artifact source reply = %#v, err=%v", messages, err)
	}
}

func TestConversationRunTurnRunnerProjectsGovernedAgentRejectionWithoutReproposal(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "agent-rejection"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"},
		Title: "Release assistant", IdempotencyKey: "agent-rejection-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Update the objective after approval.", "agent-rejection-trigger")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: "agent-42", DefinitionID: "agent-definition", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			modelCalls++
			return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{{Type: "skill_action", Capability: "openseal.objectives.update", Summary: "Propose it again"}}}, nil
		})}, nil
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
	resumed.Checkpoint = map[string]interface{}{"lastAction": map[string]interface{}{
		"status": ActionCallStatusDenied, "skillId": ObjectiveManagementSkillID, "action": ObjectiveActionUpdate,
		"arguments":  map[string]interface{}{"objectiveId": "objective-release", "expectedRevision": float64(1)},
		"approvalId": "approval-release", "approvalStatus": ApprovalStatusRejected,
		"error": "rejected: change is not authorized",
	}}
	outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: resumed})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted || modelCalls != 0 ||
		outcome.RunOutput["resourceType"] != "objective" || outcome.RunOutput["resourceId"] != "objective-release" {
		t.Fatalf("governed Agent rejection = %#v, modelCalls=%d, err=%v", outcome, modelCalls, err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 || messages[1].Content != "Objective update was not applied because approval was rejected." ||
		len(messages[1].References) != 3 || messages[1].References[1] != (ConversationReference{Kind: ConversationReferenceApproval, ID: "approval-release"}) ||
		messages[1].References[2] != (ConversationReference{Kind: ConversationReferenceObjective, ID: "objective-release"}) || messages[1].ResolvesMessageID != trigger.ID {
		t.Fatalf("governed Agent rejection message = %#v, %v", messages, err)
	}
	replayed, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: resumed})
	if err != nil || replayed.RunOutput["replayed"] != true || modelCalls != 0 {
		t.Fatalf("governed Agent rejection replay = %#v, modelCalls=%d, err=%v", replayed, modelCalls, err)
	}
}

func TestGovernedConversationActionCompletionProjectsProjectIdentity(t *testing.T) {
	run := &AgentRun{
		ID: "run-project", Checkpoint: map[string]interface{}{
			"lastAction": map[string]interface{}{
				"status": "succeeded",
				"result": map[string]interface{}{
					"resourceType": "project", "operation": "update", "created": false,
					"project": map[string]interface{}{
						"id": "project-research", "title": "Customer research", "status": "active", "revision": float64(4),
					},
				},
			},
		},
	}
	completion, ok := governedConversationActionCompletion(run)
	if !ok || completion.Content != "Project “Customer research” was updated successfully and is now active." ||
		completion.ResourceType != "project" || completion.ResourceID != "project-research" ||
		len(completion.References) != 2 ||
		completion.References[1] != (ConversationReference{Kind: ConversationReferenceProject, ID: "project-research", Version: 4}) {
		t.Fatalf("project completion = %#v, ok=%v", completion, ok)
	}
}

func TestGovernedConversationActionCompletionProjectsStartedRunbook(t *testing.T) {
	run := &AgentRun{ID: "conversation-run", Checkpoint: map[string]interface{}{
		"lastAction": map[string]interface{}{
			"status": ActionCallStatusSucceeded,
			"result": map[string]interface{}{
				"resourceType": runbookActivationResourceType,
				"operation":    RunbookActionStart,
				"activation":   map[string]interface{}{"id": "activation-hourly", "definitionId": "hourly-review"},
				"run":          map[string]interface{}{"id": "run-hourly"},
				"replayed":     false,
			},
		},
	}}
	completion, ok := governedConversationActionCompletion(run)
	if !ok || completion.ResourceType != runbookActivationResourceType || completion.ResourceID != "activation-hourly" ||
		completion.Content != "Runbook “hourly-review” was started successfully." ||
		len(completion.References) != 1 || completion.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: "run-hourly"}) {
		t.Fatalf("Runbook completion = %#v, %v", completion, ok)
	}
}

func TestGovernedConversationActionCompletionProjectsReplacedRunbookSchedule(t *testing.T) {
	run := &AgentRun{ID: "conversation-run", Checkpoint: map[string]interface{}{
		"lastAction": map[string]interface{}{
			"status": ActionCallStatusSucceeded,
			"result": map[string]interface{}{
				"resourceType": runbookActivationResourceType,
				"operation":    RunbookActionReplaceSchedule,
				"sourceActivation": map[string]interface{}{
					"id": "activation-exhausted", "definitionId": "hourly-review",
				},
				"activation": map[string]interface{}{
					"id": "activation-five-hours", "definitionId": "hourly-review",
				},
				"replayed": false,
			},
		},
	}}
	completion, ok := governedConversationActionCompletion(run)
	if !ok || completion.ResourceType != runbookActivationResourceType || completion.ResourceID != "activation-five-hours" ||
		completion.Content != "Runbook “hourly-review” now has a fresh reviewed schedule." ||
		len(completion.References) != 1 || completion.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: run.ID}) {
		t.Fatalf("Runbook replacement completion = %#v, %v", completion, ok)
	}
}

func TestGovernedConversationActionOutcomeProjectsRejectedRunbookScheduleReplacement(t *testing.T) {
	run := &AgentRun{ID: "conversation-run", Checkpoint: map[string]interface{}{
		"lastAction": map[string]interface{}{
			"status":         ActionCallStatusDenied,
			"skillId":        RunbookManagementSkillID,
			"action":         RunbookActionReplaceSchedule,
			"approvalStatus": ApprovalStatusRejected,
			"approvalId":     "approval-rejected",
		},
	}}
	completion, ok := governedConversationActionOutcome(run)
	if !ok || completion.Content != "Runbook replace schedule was not applied because approval was rejected." ||
		len(completion.References) != 2 || completion.References[1] != (ConversationReference{Kind: ConversationReferenceApproval, ID: "approval-rejected"}) {
		t.Fatalf("Runbook replacement rejection = %#v, %v", completion, ok)
	}
}

func TestGovernedConversationActionCompletionProjectsAgentBehaviorIdentity(t *testing.T) {
	run := &AgentRun{
		ID: "run-agent-behavior", Checkpoint: map[string]interface{}{
			"lastAction": map[string]interface{}{
				"status": "succeeded",
				"result": map[string]interface{}{
					"resourceType": agentBehaviorResourceType, "operation": AgentActionAmendBehavior, "replayed": false,
					"deployment": map[string]interface{}{
						"id": "researcher", "activeVersion": "1.0.0.action.abc123", "revision": float64(4),
					},
					"amendment": map[string]interface{}{
						"candidate": map[string]interface{}{"displayName": "Researcher"},
					},
				},
			},
		},
	}
	completion, ok := governedConversationActionCompletion(run)
	if !ok || completion.Content != "Agent “Researcher” behavior was updated successfully using definition 1.0.0.action.abc123." ||
		completion.ResourceType != agentBehaviorResourceType || completion.ResourceID != "researcher" ||
		len(completion.References) != 1 ||
		completion.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: run.ID}) {
		t.Fatalf("Agent behavior completion = %#v, ok=%v", completion, ok)
	}
}

func TestGovernedConversationAgentBehaviorDenialResolvesWithoutModelRetry(t *testing.T) {
	run := &AgentRun{
		ID: "run-agent-denied", Kind: RunKindConversation,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"},
		Checkpoint: map[string]interface{}{"lastAction": map[string]interface{}{
			"status": ActionCallStatusDenied, "skillId": AgentManagementSkillID, "action": AgentActionAmendBehavior,
			"arguments":  map[string]interface{}{"personality": "More concise", "rationale": "Reduce noise"},
			"approvalId": "approval-agent", "approvalStatus": ApprovalStatusRejected,
		}},
	}
	outcome, ok := governedConversationActionOutcome(run)
	if !ok || outcome.Content != "Agent behavior was not applied because approval was rejected." ||
		outcome.ResourceType != agentBehaviorResourceType || outcome.ResourceID != "researcher" ||
		len(outcome.References) != 2 ||
		outcome.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: run.ID}) ||
		outcome.References[1] != (ConversationReference{Kind: ConversationReferenceApproval, ID: "approval-agent"}) {
		t.Fatalf("Agent behavior denial = %#v, ok=%v", outcome, ok)
	}
}

func TestGovernedConversationProposalRejectionIsReturnedWithoutAMutationOutcome(t *testing.T) {
	run := &AgentRun{
		ID: "run-pause", Kind: RunKindConversation, Scope: Scope{Kind: "tenant", ID: "1"},
		Checkpoint: map[string]interface{}{"phase": "before-pause"},
	}
	turn := &AgentTurn{ContinuationCheckpoint: map[string]interface{}{
		"actionInputs": map[string]interface{}{"call": map[string]interface{}{"objectiveId": "objective-draft", "expectedRevision": float64(2)}},
	}, RequestedActions: []TurnAction{{
		Type: "skill_action", Capability: "openseal.objectives.pause", InputRef: "/actionInputs/call",
		BindingID: "bundled:objectives", BindingRevision: 1,
	}}}
	for _, kind := range []RunKind{RunKindConversation, RunKindAgentWork} {
		current := cloneAgentRun(run)
		current.Kind = kind
		checkpoint, accepted := checkpointProposalRejection(current, turn, sanitizeActionError(fmt.Errorf("%w: draft -> paused", ErrInvalidObjectiveTransition), nil))
		feedback, active := ReadToolFeedbackCorrection(checkpoint)
		if !accepted || !active || feedback.Kind != "proposal" || !strings.Contains(feedback.Message, "draft -> paused") || checkpoint["actionInputs"] != nil {
			t.Fatalf("rejected proposal was not returned to the model: %#v accepted=%v", checkpoint, accepted)
		}
		if current.Checkpoint["phase"] != "before-pause" || current.Checkpoint[ToolFeedbackCorrectionCheckpointKey] != nil || requiresFinalFailureExplanation(current.Checkpoint) {
			t.Fatalf("rejection mutated the trusted checkpoint: %#v", current.Checkpoint)
		}
		// A proposal which never materialized cannot fabricate a completed mutation.
		current.Checkpoint = checkpoint
		if outcome, projected := governedConversationActionOutcome(current); projected {
			t.Fatalf("unexecuted proposal was reported as a governed mutation: %#v", outcome)
		}
	}
}

func TestGovernedConversationActionFailureProjectsStaleRevision(t *testing.T) {
	run := &AgentRun{
		ID: "run-stale-project", Kind: RunKindConversation, Scope: Scope{Kind: "tenant", ID: "1"},
		Checkpoint: map[string]interface{}{"lastAction": map[string]interface{}{
			"actionCallId": "call-stale-project", "status": ActionCallStatusFailed,
			"skillId": ProjectManagementSkillID, "action": ObjectiveActionPause,
			"arguments": map[string]interface{}{
				"projectId": "project-research", "expectedRevision": float64(1),
			},
			"approvalId": "approval-stale-project", "error": "project revision conflict",
		}},
	}
	outcome, ok := governedConversationActionOutcome(run)
	if !ok || outcome.Content != "Project pause was not applied because the Project changed while approval was pending. Review the latest state and try again." ||
		outcome.ResourceType != "project" || outcome.ResourceID != "project-research" || len(outcome.References) != 3 ||
		outcome.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: run.ID}) ||
		outcome.References[1] != (ConversationReference{Kind: ConversationReferenceApproval, ID: "approval-stale-project"}) ||
		outcome.References[2] != (ConversationReference{Kind: ConversationReferenceProject, ID: "project-research"}) {
		t.Fatalf("stale Project outcome = %#v, ok=%v", outcome, ok)
	}
}

func TestConversationRunReconciliationDoesNotReplayLegacyCoordinatedMessages(t *testing.T) {
	store := NewMemoryStore()
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
	if _, err := coordinator.Coordinate(ctx, ConversationRoutingRequest{
		Scope: scope, ConversationID: conversation.ID, TriggerMessageID: trigger.ID, IdempotencyKey: "legacy-manual-round",
	}); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := scheduler.ReconcileScope(ctx, scope)
	if err != nil || result.Messages != 1 || result.Scheduled != 0 || result.Skipped != 1 {
		t.Fatalf("legacy reconciliation = %#v, %v", result, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation})
	if err != nil || len(runs) != 0 {
		t.Fatalf("legacy messages were replayed as Runs: %#v, %v", runs, err)
	}
}

// conversationRunTestCoordinator routes every unaddressed message to agent-1.
func conversationRunTestCoordinator(t *testing.T, service *ConversationService) *ConversationCoordinator {
	t.Helper()
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{
			Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"},
		}}, nil
	})
	router := ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
		return ConversationRouteDecision{AgentID: "agent-1", Reason: "owns the canary"}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, router, DefaultConversationCoordinatorConfig())
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
