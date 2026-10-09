package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type conversationFollowUpFixture struct {
	store        *MemoryStore
	service      *ConversationService
	scheduler    *ConversationRunScheduler
	conversation *Conversation
}

func newConversationFollowUpFixture(t *testing.T) *conversationFollowUpFixture {
	t.Helper()
	store := NewMemoryStore()
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: Scope{Kind: "tenant", ID: "follow-ups"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"},
		Title: "Shopping assistant", IdempotencyKey: "follow-up-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conversationFollowUpFixture{store: store, service: service, scheduler: mustConversationRunScheduler(t, store), conversation: conversation}
}

func (f *conversationFollowUpFixture) post(t *testing.T, sender, content string) *ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: f.conversation.Scope, ConversationID: f.conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: sender}, Intent: MessageIntentQuestion,
		Content: content, RequiresResponse: true, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: "message:" + content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

func (f *conversationFollowUpFixture) schedule(t *testing.T, message *ChannelMessage) *AgentRun {
	t.Helper()
	result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.conversation.Scope, f.conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule %s = %#v, %v", message.ID, result, err)
	}
	return result.Run
}

func (f *conversationFollowUpFixture) run(t *testing.T, id string) *AgentRun {
	t.Helper()
	run, err := f.store.GetAgentRun(t.Context(), f.conversation.Scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

func (f *conversationFollowUpFixture) conversationRuns(t *testing.T) []*AgentRun {
	t.Helper()
	runs, err := f.store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: f.conversation.Scope, Kind: RunKindConversation, Owner: &f.conversation.Owner})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (f *conversationFollowUpFixture) transition(t *testing.T, run *AgentRun, status AgentRunStatus, wake *WakeCondition) *AgentRun {
	t.Helper()
	next, _, err := NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, WakeCondition: wake,
	})
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func (f *conversationFollowUpFixture) runner(t *testing.T, turn func(TurnExecutionContext) *TurnOutcome) *ConversationRunTurnRunner {
	t.Helper()
	runner, err := NewConversationRunTurnRunner(f.store, conversationRunTestCoordinator(t, f.service), ConversationRunTurnRunnerConfig{
		AgentTurns: TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
			return &TurnRunnerBinding{DeploymentID: "agent-42", Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				return turn(input), nil
			})}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

type conversationFollowUpGoal struct {
	Messages         []agentConversationPromptMessage `json:"messages"`
	CurrentMessage   agentConversationPromptMessage   `json:"currentMessage"`
	FollowUpMessages []agentConversationPromptMessage `json:"followUpMessages"`
}

func decodeConversationFollowUpGoal(t *testing.T, goal string) conversationFollowUpGoal {
	t.Helper()
	instructions, raw, ok := strings.Cut(goal, "\n\n")
	if !ok || !strings.Contains(instructions, "followUpMessages") {
		t.Fatalf("goal does not explain follow-ups: %s", goal)
	}
	var payload conversationFollowUpGoal
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func completedConversationAnswer(summary string) *TurnOutcome {
	return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: summary, RunOutput: map[string]interface{}{"summary": summary}}
}

// Three rapid messages from one person in one lane produce one Run. The Run's
// next Turn answers the first message with the later two as follow-ups.
func TestRapidConversationMessagesShareOneRunThatSeesAll(t *testing.T) {
	f := newConversationFollowUpFixture(t)
	first := f.post(t, "user", "Find the cheapest USB-C hub on Amazon")
	second := f.post(t, "user", "It must have HDMI")
	third := f.post(t, "user", "Use my card ending with 1001 to buy it")
	run := f.schedule(t, first)
	for _, message := range []*ChannelMessage{second, third} {
		if delivered := f.schedule(t, message); delivered.ID != run.ID {
			t.Fatalf("message %s started a parallel Run %s", message.ID, delivered.ID)
		}
	}
	if runs := f.conversationRuns(t); len(runs) != 1 {
		t.Fatalf("conversation Runs = %d, want 1", len(runs))
	}
	run = f.run(t, run.ID)
	followUps := conversationRunFollowUps(run)
	if run.Status != AgentRunStatusQueued || len(followUps) != 2 || followUps[0].MessageID != second.ID || followUps[1].MessageID != third.ID ||
		run.PendingInterventions[0].Instruction != second.Content || run.PendingInterventions[1].Actor != (ActivityActor{Type: "user", ID: "user"}) {
		t.Fatalf("follow-ups were not delivered in order: %#v", run)
	}

	// Recovery replays every message without new Runs or duplicate delivery.
	reconciled, err := f.scheduler.ReconcileScope(t.Context(), f.conversation.Scope)
	if err != nil || reconciled.Scheduled != 0 || reconciled.Replayed != 3 {
		t.Fatalf("reconciliation = %#v, %v", reconciled, err)
	}
	if runs := f.conversationRuns(t); len(runs) != 1 || len(runs[0].PendingInterventions) != 2 {
		t.Fatalf("replay duplicated work: %#v", runs)
	}

	modelCalls := 0
	runner := f.runner(t, func(input TurnExecutionContext) *TurnOutcome {
		modelCalls++
		goal := decodeConversationFollowUpGoal(t, input.Run.Goal)
		if goal.CurrentMessage.ID != first.ID || len(goal.FollowUpMessages) != 2 ||
			goal.FollowUpMessages[0].Content != second.Content || goal.FollowUpMessages[1].Content != third.Content {
			t.Fatalf("Turn did not see all three messages: %#v", goal)
		}
		for _, message := range goal.Messages {
			if message.ID == second.ID || message.ID == third.ID {
				t.Fatalf("follow-up was presented as past context: %#v", goal.Messages)
			}
		}
		if len(input.Run.PendingInterventions) != 2 {
			t.Fatalf("hosted Turn lost pending input: %#v", input.Run.PendingInterventions)
		}
		return completedConversationAnswer("Bought the Anker hub with HDMI using the card ending 1001.")
	})
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run})
	if err != nil || modelCalls != 1 || outcome.NextRunStatus != AgentRunStatusCompleted {
		t.Fatalf("Turn outcome = %#v, calls=%d, err=%v", outcome, modelCalls, err)
	}
	answer, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), f.conversation.Scope, f.conversation.ID, conversationTaskFinalResponseKey(run))
	if err != nil || answer == nil || answer.ReplyToMessageID != first.ID || answer.ResolvesMessageID != first.ID || outcome.RunOutput["messageId"] != answer.ID {
		t.Fatalf("answer = %#v, %v", answer, err)
	}

	// A terminal Run never absorbs: the next message starts a new Run.
	run = f.transition(t, f.transition(t, f.run(t, run.ID), AgentRunStatusRunning, nil), AgentRunStatusCompleted, nil)
	fourth := f.post(t, "user", "Now track the delivery")
	next := f.schedule(t, fourth)
	if next.ID == run.ID || next.Context[conversationRunContextTriggerID] != fourth.ID || len(next.PendingInterventions) != 0 {
		t.Fatalf("terminal Run absorbed a new message: %#v", next)
	}
	if current := f.run(t, run.ID); len(current.PendingInterventions) != 2 {
		t.Fatalf("terminal Run changed: %#v", current)
	}
}

// Waiting Runs keep waiting on their approval, credential, browser handoff or
// pause; the message is still durable on the Run for its next Turn. Only a
// sleeping retry or schedule timer is cut short by fresh human input.
func TestConversationFollowUpDeliveryPreservesWaitingStates(t *testing.T) {
	wakeAt := time.Now().Add(time.Hour).UTC()
	for _, test := range []struct {
		status AgentRunStatus
		wake   *WakeCondition
		want   AgentRunStatus
	}{
		{status: AgentRunStatusRunning, want: AgentRunStatusRunning},
		{status: AgentRunStatusSleeping, wake: &WakeCondition{Type: "timer", WakeAt: &wakeAt}, want: AgentRunStatusQueued},
		{status: AgentRunStatusWaitingForEvent, wake: &WakeCondition{Type: "human_intervention", Reference: "credential-request"}, want: AgentRunStatusWaitingForEvent},
		{status: AgentRunStatusWaitingForApproval, wake: &WakeCondition{Type: "approval", Reference: "approval-1"}, want: AgentRunStatusWaitingForApproval},
		{status: AgentRunStatusPaused, want: AgentRunStatusPaused},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			f := newConversationFollowUpFixture(t)
			run := f.schedule(t, f.post(t, "user", "Buy the hub"))
			run = f.transition(t, run, AgentRunStatusRunning, nil)
			if test.status != AgentRunStatusRunning {
				run = f.transition(t, run, test.status, test.wake)
			}
			message := f.post(t, "user", "Actually use the other card")
			delivered := f.schedule(t, message)
			if delivered.ID != run.ID || delivered.Status != test.want || !conversationRunHasFollowUp(delivered, message.ID) {
				t.Fatalf("delivered Run = %#v", delivered)
			}
			if test.want == test.status && (test.wake == nil) != (delivered.WakeCondition == nil) {
				t.Fatalf("wake condition changed: %#v", delivered.WakeCondition)
			}
			if test.want == AgentRunStatusQueued && delivered.WakeCondition != nil {
				t.Fatalf("queued Run kept a stale timer: %#v", delivered.WakeCondition)
			}
			if len(f.conversationRuns(t)) != 1 {
				t.Fatal("waiting Run did not absorb the message")
			}
		})
	}
}

func TestConversationFollowUpSkipsRunExplainingItsFailure(t *testing.T) {
	f := newConversationFollowUpFixture(t)
	run := f.schedule(t, f.post(t, "user", "Buy the hub"))
	run = f.transition(t, run, AgentRunStatusRunning, nil)
	run, _, err := NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning,
		Checkpoint: checkpointFinalFailureExplanation(map[string]interface{}{}, "tool", "checkout failed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	message := f.post(t, "user", "Try again with the other card")
	next := f.schedule(t, message)
	if next.ID == run.ID || next.Context[conversationRunContextTriggerID] != message.ID {
		t.Fatalf("failing Run absorbed a new request: %#v", next)
	}
}

// A message delivered while a Turn executes supersedes that Turn's answer:
// nothing is posted, and the continued Run answers with the new input.
func TestConversationFollowUpDuringTurnSupersedesItsAnswer(t *testing.T) {
	f := newConversationFollowUpFixture(t)
	first := f.post(t, "user", "Find a USB-C hub")
	run := f.schedule(t, first)
	var second *ChannelMessage
	runner := f.runner(t, func(TurnExecutionContext) *TurnOutcome {
		second = f.post(t, "user", "Only ones with HDMI")
		f.schedule(t, second)
		return completedConversationAnswer("Here is a hub without HDMI.")
	})
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run})
	if err != nil || outcome == nil || outcome.RunOutput["messageId"] != nil {
		t.Fatalf("superseded outcome = %#v, %v", outcome, err)
	}
	messages, err := f.service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.conversation.Scope, ConversationID: f.conversation.ID})
	if err != nil || len(messages) != 2 {
		t.Fatalf("stale answer was posted: %#v, %v", messages, err)
	}
	continued := f.run(t, run.ID)
	runner = f.runner(t, func(input TurnExecutionContext) *TurnOutcome {
		if goal := decodeConversationFollowUpGoal(t, input.Run.Goal); len(goal.FollowUpMessages) != 1 || goal.FollowUpMessages[0].ID != second.ID {
			t.Fatalf("continued Turn missed the follow-up: %#v", goal)
		}
		return completedConversationAnswer("Here is a hub with HDMI.")
	})
	outcome, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: continued})
	if err != nil || outcome.RunOutput["messageId"] == nil {
		t.Fatalf("continued outcome = %#v, %v", outcome, err)
	}
	if conversationTaskFinalResponseKey(continued) == conversationTaskFinalResponseKey(run) {
		t.Fatal("an answer after a follow-up reuses the earlier answer key")
	}
}

func TestExternalInboxFollowUpItemDefersToTriggerItem(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "inbox"}
	run := &AgentRun{Scope: scope, Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "trigger"}}
	run.PendingInterventions = []AgentRunIntervention{{
		ID:                  conversationFollowUpInterventionID(scope, "follow-up"),
		ConversationMessage: &ConversationMessageDelivery{ConversationID: "chat", MessageID: "follow-up"},
	}, {
		ID: "operator-guidance", Instruction: "Keep going",
	}}
	if externalInboxItemIsFollowUp(&ExternalConversationInboxItem{ChannelMessageID: "trigger"}, run) {
		t.Fatal("trigger item treated as follow-up")
	}
	if !externalInboxItemIsFollowUp(&ExternalConversationInboxItem{ChannelMessageID: "follow-up"}, run) {
		t.Fatal("follow-up item would duplicate the Run's reply")
	}
	if externalInboxItemIsFollowUp(&ExternalConversationInboxItem{ChannelMessageID: "unrelated"}, run) {
		t.Fatal("unrelated item treated as follow-up")
	}
}
