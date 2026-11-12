package externalconversationtest

import (
	"testing"
	"time"

	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/google/uuid"
)

// TerminalReportingStore is a store with runs, their terminal report queue
// and conversations.
type TerminalReportingStore interface {
	models.RunCommandStore
	models.RunTerminalReportingStore
	models.ConversationStore
}

// TerminalReportingStoreOf is store as a TerminalReportingStore; the test
// fails when store has no terminal reporting queue.
func TerminalReportingStoreOf(t *testing.T, store interface{}) TerminalReportingStore {
	t.Helper()
	reporting, ok := store.(TerminalReportingStore)
	if !ok {
		t.Fatal("canonical persistent store lacks terminal reporting queue")
	}
	return reporting
}

// TerminalReportingRun creates a conversation with a person's request and a
// scheduled Run, created at at, that reports its completion or failure
// there.
func TerminalReportingRun(t *testing.T, store TerminalReportingStore, scope models.Scope, at time.Time) (*models.AgentRun, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "report-agent"}
	conversations := runtimeconversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := conversations.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Original request", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := conversations.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requesting-user"}, Intent: models.MessageIntentQuestion,
		Content: "Send me the result here when the work finishes", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	commands := agentrun.NewRunCommandService(store, models.WithClock(func() time.Time { return at }))
	created, err := commands.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Report the completed background work", Source: models.RunSourceSchedule, IdempotencyKey: key,
		Context: map[string]interface{}{
			models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: trigger.Message.ID,
			models.RunReportingRootRunIDContextKey: agentrun.RunIDForIdempotencyKey(scope, key), models.RunReportingMilestonesContextKey: []string{"completed", "failed"}, models.ScheduledTaskContextKey: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.Run, trigger.Conversation, trigger.Message
}

// CompleteTerminalReportingRun moves run to running and then completes it
// with a summary, at at.
func CompleteTerminalReportingRun(t *testing.T, store TerminalReportingStore, run *models.AgentRun, at time.Time) *models.AgentRun {
	t.Helper()
	activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return at }))
	running, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusCompleted, Output: map[string]interface{}{"summary": "The requested result is ready"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

// TerminalReportingWorker is a terminal reporting worker on store whose
// clock reads at.
func TerminalReportingWorker(t *testing.T, store TerminalReportingStore, at time.Time) *externalconversation.RunTerminalReportingWorker {
	t.Helper()
	return TerminalReportingWorkerWithClock(t, store, func() time.Time { return at })
}

// TerminalReportingWorkerWithClock is TerminalReportingWorker reading time
// from clock.
func TerminalReportingWorkerWithClock(t *testing.T, store TerminalReportingStore, clock models.Clock) *externalconversation.RunTerminalReportingWorker {
	t.Helper()
	worker, err := externalconversation.NewRunTerminalReportingWorker(store, externalconversation.RunTerminalReportingWorkerOptions{WorkerID: "report-worker", LeaseDuration: time.Minute, BatchSize: 10, Concurrency: 2}, models.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

// AssertTerminalReport checks that the conversation holds the trigger and
// exactly one report of run, posted to the channel and referencing run.
func AssertTerminalReport(t *testing.T, store models.ConversationStore, run *models.AgentRun, conversation *models.Conversation, trigger *models.ChannelMessage) {
	t.Helper()
	messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: run.Scope, ConversationID: conversation.ID, Limit: 100})
	if err != nil || len(messages) != 2 {
		t.Fatalf("terminal result lost or duplicated: messages=%#v error=%v", messages, err)
	}
	var report *models.ChannelMessage
	for _, message := range messages {
		if message.ID != trigger.ID {
			report = message
		}
	}
	if report == nil || report.Content != "The requested result is ready" || report.ReplyToMessageID != "" || report.ThreadRootID != "" || len(report.References) != 1 || report.References[0].ID != run.ID {
		t.Fatalf("result missed the original chat/context: %#v", report)
	}
}
