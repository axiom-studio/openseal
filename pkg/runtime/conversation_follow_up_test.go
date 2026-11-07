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
