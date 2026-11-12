package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestConversationRunSchedulerIsIdempotentAndReconcilesMissedMessages(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "scheduler"}
	service := runtimeconversation.NewConversationService(store)
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"},
		Title: "Engineering", IdempotencyKey: "engineering-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := conversationruntest.PostMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Is the canary healthy?", "message-one")
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{ConversationPageSize: 1, MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	created, replayed, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, first.ID)
	if err != nil || replayed || created == nil || created.Run == nil {
		t.Fatalf("initial schedule = %#v, replayed=%v, err=%v", created, replayed, err)
	}
	if created.Event == nil || created.Run.Kind != RunKindConversation || created.Run.ConcurrencyKey != ConversationRunLane(conversation.ID, "", created.Run.AssignedAgentID) ||
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
	second := conversationruntest.PostMessage(t, service, current, ConversationParticipantUser, MessageIntentQuestion, "Please review the release notes.", "message-two")
	current, err = service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	conversationruntest.PostMessage(t, service, current, ConversationParticipantService, MessageIntentSystem, "Channel metadata synchronized.", "message-system")

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
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "restart"}
	service := runtimeconversation.NewConversationService(store)
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
		Title: "Operations", IdempotencyKey: "operations-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := conversationruntest.PostMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "What is failing?", "restart-one")
	current, err := service.GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := conversationruntest.PostMessage(t, service, current, ConversationParticipantUser, MessageIntentUpdate, "The database is healthy.", "restart-two")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := conversationrun.NewConversationRunScheduler(reopened, reopened, conversationrun.ConversationRunSchedulerConfig{ConversationPageSize: 10, MessagePageSize: 1})
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

	verified, err := sqlite.NewSQLiteStore(path)
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
	cursor, err := runtimeconversation.NewConversationService(verified).GetCursor(ctx, scope, conversation.ID, ConversationParticipant{
		Type: ConversationParticipantService, ID: conversationRunSchedulerParticipant,
	})
	if err != nil || cursor == nil || cursor.ReadSequence != 2 || cursor.DeliveredSequence != 2 {
		t.Fatalf("persisted scheduler cursor = %#v, %v", cursor, err)
	}
}

func TestConversationRunSchedulerProjectsCanceledRequestExactlyOnce(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "canceled-conversation"}
	service := runtimeconversation.NewConversationService(store)
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "operations"},
		Title: "Operations", IdempotencyKey: "canceled-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"},
		Intent: MessageIntentQuestion, Content: "Pause the project.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "canceled-trigger",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := conversationruntest.NewScheduler(t, store)
	scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, posted.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := agentrun.NewRunCommandService(store).CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
		Scope: scope, RunID: scheduled.Run.ID, ExpectedRevision: scheduled.Run.Revision, Kind: agentrun.AgentRunCommandCancel,
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

func TestConversationRunReconciliationDoesNotReplayLegacyCoordinatedMessages(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "legacy-round"}
	service := runtimeconversation.NewConversationService(store)
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"},
		Title: "Engineering", IdempotencyKey: "legacy-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := conversationruntest.PostMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Is the release ready?", "legacy-trigger")
	coordinator := conversationRunTestCoordinator(t, service)
	if _, err := coordinator.Coordinate(ctx, runtimeconversation.ConversationRoutingRequest{
		Scope: scope, ConversationID: conversation.ID, TriggerMessageID: trigger.ID, IdempotencyKey: "legacy-manual-round",
	}); err != nil {
		t.Fatal(err)
	}
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
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
func conversationRunTestCoordinator(t *testing.T, service *runtimeconversation.ConversationService) *runtimeconversation.ConversationCoordinator {
	t.Helper()
	participants := runtimeconversation.ConversationParticipantSourceFunc(func(context.Context, runtimeconversation.ConversationParticipantQuery) ([]runtimeconversation.ConversationParticipantBinding, error) {
		return []runtimeconversation.ConversationParticipantBinding{{
			Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"},
		}}, nil
	})
	router := runtimeconversation.ConversationRouterFunc(func(context.Context, runtimeconversation.ConversationRouteRequest) (runtimeconversation.ConversationRouteDecision, error) {
		return runtimeconversation.ConversationRouteDecision{AgentID: "agent-1", Reason: "owns the canary"}, nil
	})
	coordinator, err := runtimeconversation.NewConversationCoordinator(service, participants, router, runtimeconversation.DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func conversationRunRoundKey(scope Scope, conversationID, triggerID string) string {
	return "participation-round:" + hashString(scope.Kind+"\x00"+scope.ID+"\x00"+conversationID+"\x00"+triggerID)
}

// chatRunResolver is the Agent's own loop binding as ResolveLoopBinding gives
// it to a chat Run: the model producer, the run-start preparer and the
// control authority of the speaking Agent. It records the Runs it resolves.
type chatRunResolver struct {
	store    chatLoopStore
	producer agentrun.StepProducer
	actions  []capability.ModelAction
	resolved []*AgentRun
}

func (r *chatRunResolver) ResolveTurnRunner(_ context.Context, run *AgentRun) (*agentrun.TurnRunnerBinding, error) {
	r.resolved = append(r.resolved, cloneAgentRun(run))
	return &agentrun.TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, DefinitionID: "agent-definition", DefinitionVersion: "7",
		ModelProvider: string(ModelStepProviderAnthropic), Model: "chat-model",
		ModelActions: append([]capability.ModelAction(nil), r.actions...),
		Producer:     r.producer, Preparers: []agentrun.StepPreparer{NewRunStartPreparer(r.store, r.store)},
		Authority: agentrun.ControlAuthority{AgentID: run.AssignedAgentID, CanAskUser: true, CanReact: true},
	}, nil
}

func (f *chatLoopFixture) chatRunner(resolver runtimeconversation.TurnRunnerResolver, participation bool) *conversationrun.ConversationRunTurnRunner {
	f.t.Helper()
	runner, err := conversationrun.NewConversationRunTurnRunner(f.store, conversationRunTestCoordinator(f.t, f.service), conversationrun.ConversationRunTurnRunnerConfig{
		AgentTurns: resolver, RequireParticipationOptIn: participation,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return runner
}

func TestConversationRunBindsAgentChatOnLoopSeams(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		resolver := &chatRunResolver{store: f.store, producer: &chatLoopProducer{answers: []string{"The release evidence is healthy."}}}
		runner := f.chatRunner(resolver, false)
		trigger := f.post("release", "user-1", "Summarize the release evidence.")
		run := f.startRun(trigger)
		binding, err := runner.ResolveTurnRunner(f.ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		// The Agent resolver binds the canonical conversation Run itself.
		if len(resolver.resolved) != 1 || resolver.resolved[0].Kind != RunKindConversation || resolver.resolved[0].ID != run.ID {
			t.Fatalf("resolved Runs = %#v", resolver.resolved)
		}
		if binding.Producer != resolver.producer || binding.Session == nil || binding.Session.ID != run.SessionID {
			t.Fatalf("binding = %#v", binding)
		}
		// The resolver's binding has no Final: the chat seams add the observed
		// final delivery.
		if binding.Final == nil || binding.ToolCheck == nil || binding.AfterTool == nil {
			t.Fatalf("conversation seams = final %T, check %v, after %v", binding.Final, binding.ToolCheck != nil, binding.AfterTool != nil)
		}
		if len(binding.Preparers) != 2 || len(binding.Prompt.Sections) == 0 {
			t.Fatalf("preparers = %d, prompt sections = %d", len(binding.Preparers), len(binding.Prompt.Sections))
		}
		exit := f.drive(run, binding)
		if exit.Reason != RunLoopExitFinal {
			t.Fatalf("exit = %#v", exit)
		}
		done := f.run(run.ID)
		messages := f.agentMessages()
		if done.Status != AgentRunStatusCompleted || len(messages) != 1 {
			t.Fatalf("run = %s %#v, agent messages = %#v", done.Status, done.Output, messages)
		}
		answer := messages[0]
		if answer.Content != "The release evidence is healthy." || answer.ReplyToMessageID != trigger.ID ||
			answer.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}) ||
			len(answer.References) == 0 || answer.References[0].Kind != ConversationReferenceRun || answer.References[0].ID != run.ID {
			t.Fatalf("answer = %#v", answer)
		}
		if done.Output["messageId"] != answer.ID || done.Output["triggerMessageId"] != trigger.ID {
			t.Fatalf("output = %#v", done.Output)
		}
		// The answer is the Agent's own assistant entry in its lane session; no
		// goal text reached the model.
		path := f.path(run.SessionID)
		assistant := 0
		for _, entry := range path {
			if entry.Type == SessionEntryAssistant && entry.RunID == run.ID {
				assistant++
			}
			if entry.Type == SessionEntryUser && strings.Contains(chatLoopEntryText(entry), "currentMessage") {
				t.Fatalf("goal JSON in the session: %#v", entry)
			}
		}
		if assistant != 1 {
			t.Fatalf("assistant entries = %d in %d", assistant, len(path))
		}
	})
}

func TestTeamRoutingRunCompletesWithResponderRunIds(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{team: true}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("route", "user-1", "Who can check the canary?")
		commands := agentrun.NewRunCommandService(f.store, models.WithClock(func() time.Time { return f.now }))
		created, err := commands.CreateAgentRun(f.ctx, runtimeconversation.ConversationAgentRunRequest(f.conversation, trigger))
		if err != nil {
			t.Fatal(err)
		}
		if created.Run.SessionID != "" || created.Run.AssignedAgentID != "" {
			t.Fatalf("routing Run = %#v", created.Run)
		}
		claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{
			Scope: f.conversation.Scope, WorkerID: chatLoopWorker, Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
		})
		if err != nil || claimed == nil || claimed.ID != created.Run.ID {
			t.Fatalf("claim = %#v, %v", claimed, err)
		}
		binding, err := f.runner.ResolveTurnRunner(f.ctx, claimed)
		if err != nil {
			t.Fatal(err)
		}
		// The router's own binding: the drive below shows its routing output.
		if binding.DefinitionID != "openseal.conversation-router" || binding.Producer == nil || binding.Session != nil || binding.Final != nil || len(binding.Preparers) != 0 {
			t.Fatalf("routing binding = %#v", binding)
		}
		if exit := f.drive(claimed, binding); exit.Reason != RunLoopExitFinal {
			t.Fatalf("exit = %#v", exit)
		}
		done := f.run(claimed.ID)
		ids, _ := done.Output["responderRunIds"].([]interface{})
		if done.Status != AgentRunStatusCompleted || len(ids) != 1 || done.Output["conversationId"] != f.conversation.ID ||
			done.Output["triggerMessageId"] != trigger.ID || done.Output["participationRoundId"] == "" || done.Output["replayed"] != false {
			t.Fatalf("routing Run = %s %#v", done.Status, done.Output)
		}
		member := f.run(fmt.Sprint(ids[0]))
		lane := ConversationRunLane(f.conversation.ID, "", "agent-1")
		if member.Kind != RunKindConversation || member.AssignedAgentID != "agent-1" || member.ConcurrencyKey != lane ||
			member.SessionID != conversationSessionID(f.conversation.Scope, lane) || member.Context[runtimeconversation.TeamRoutingRunContextKey] != claimed.ID {
			t.Fatalf("member Run = %#v", member)
		}
		if len(f.agentMessages()) != 0 {
			t.Fatalf("the routing Run posted: %#v", f.agentMessages())
		}
	})
}

func TestConversationRunOutsideParticipationEndsWithoutAgentBinding(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{participation: true}, func(t *testing.T, f *chatLoopFixture) {
		resolver := &chatRunResolver{store: f.store, producer: &chatLoopProducer{}}
		runner := f.chatRunner(resolver, true)
		run := f.startRun(f.post("quiet", "user-1", "Anyone there?"))
		binding, err := runner.ResolveTurnRunner(f.ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		// The participation stop's fixed-output binding, not the Agent's: the
		// drive below shows its participationSkipped output.
		if binding.Model != "participation-disabled" || binding.Producer == nil || len(resolver.resolved) != 0 || binding.Session != nil {
			t.Fatalf("binding = %#v, resolved = %d", binding, len(resolver.resolved))
		}
		if exit := f.drive(run, binding); exit.Reason != RunLoopExitFinal {
			t.Fatalf("exit = %#v", exit)
		}
		done := f.run(run.ID)
		if done.Status != AgentRunStatusCompleted || done.Output["participationSkipped"] != true || len(f.agentMessages()) != 0 {
			t.Fatalf("run = %s %#v, messages = %#v", done.Status, done.Output, f.agentMessages())
		}
	})
}

func TestConversationRunPinnedRunbookKeepsAgentBindingWithoutChatSeams(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		resolver := &chatRunResolver{store: f.store, producer: &chatLoopProducer{}}
		runner := f.chatRunner(resolver, false)
		run := cloneAgentRun(f.startRun(f.post("pinned", "user-1", "Handle this through the runbook.")))
		run.Plan = map[string]interface{}{"runbook": map[string]interface{}{"id": "handler", "version": "1"}}
		binding, err := runner.ResolveTurnRunner(f.ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolver.resolved) != 1 || binding.Producer != resolver.producer || binding.Final != nil || binding.ToolCheck != nil ||
			binding.AfterTool != nil || binding.Session != nil || len(binding.Preparers) != 1 {
			t.Fatalf("pinned runbook binding = %#v", binding)
		}
	})
}

func TestConversationRunRefusesAgentBindingWithoutProducer(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		runner := f.chatRunner(TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*agentrun.TurnRunnerBinding, error) {
			return &agentrun.TurnRunnerBinding{DeploymentID: "agent-1"}, nil
		}), false)
		run := f.startRun(f.post("broken", "user-1", "Hello"))
		if _, err := runner.ResolveTurnRunner(f.ctx, run); !errors.Is(err, runtimeconversation.ErrConversationCoordinationUnavailable) {
			t.Fatalf("err = %v", err)
		}
		withoutResolver := f.chatRunner(nil, false)
		if _, err := withoutResolver.ResolveTurnRunner(f.ctx, run); !errors.Is(err, runtimeconversation.ErrConversationCoordinationUnavailable) {
			t.Fatalf("err without an Agent resolver = %v", err)
		}
	})
}
