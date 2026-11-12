package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation/externalconversationtest"
)

func TestExternalConversationInboxWorkerRecoversWithoutDuplicateMessageOrRun(t *testing.T) {
	ctx := context.Background()
	store, _, endpoint := externalconversationtest.EndpointFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator,
	})
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := &ExternalConversationInboxItem{
		ID: "external-inbox-event", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "Ev/+=1", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "workspace/channel", ExternalThreadID: "171.001",
			ExternalMessageID: "171.002", ExternalParticipantID: "U/123",
			ParticipantDisplayName: "Ada", Text: "Can you investigate this?", MentionsEndpoint: true,
			OrderingKey: "workspace/channel:171.001", OccurredAt: now,
		},
		Status: ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, replayed, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil || replayed {
		t.Fatalf("receive = replayed %v, %v", replayed, err)
	}
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &externalconversationtest.CrashAfterDispatch{
		Next: externalconversation.NewCanonicalExternalConversationDispatcher(scheduler), CrashOnce: true,
	}
	workerClock := time.Now
	worker, err := externalconversation.NewExternalConversationInboxWorker(store, dispatcher, externalconversation.ExternalConversationInboxWorkerConfig{
		WorkerID: "inbox-worker", LeaseDuration: time.Minute, BaseRetry: time.Second, MaximumRetry: time.Minute,
	}, models.WithClock(func() time.Time { return workerClock() }))
	if err != nil {
		t.Fatal(err)
	}
	workerClock = func() time.Time { return now }

	failed, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err == nil || failed == nil || failed.Status != ExternalConversationInboxRetry || failed.Attempt != 1 {
		t.Fatalf("synthetic crash = %#v, %v", failed, err)
	}
	conversations, err := store.ListConversations(ctx, ConversationFilter{Scope: endpoint.Scope, Limit: 10})
	if err != nil || len(conversations) != 1 {
		t.Fatalf("conversations after crash = %#v, %v", conversations, err)
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversations[0].ID, Limit: 10,
	})
	if err != nil || len(messages) != 1 || messages[0].SenderDisplayName != "Ada" || !messages[0].RequiresResponse {
		t.Fatalf("messages after crash = %#v, %v", messages, err)
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: endpoint.Scope, Kind: RunKindConversation, Limit: 10})
	if err != nil || len(runs) != 1 || runs[0].ID != dispatcher.RunID {
		t.Fatalf("Runs after crash = %#v, %v", runs, err)
	}

	workerClock = func() time.Time { return now.Add(time.Second) }
	applied, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil || applied == nil || applied.Status != ExternalConversationInboxApplied ||
		applied.ChannelMessageID != messages[0].ID || applied.RunID != runs[0].ID || applied.Attempt != 2 {
		t.Fatalf("recovered application = %#v, %v", applied, err)
	}
	if dispatcher.Dispatches != 2 {
		t.Fatalf("dispatch attempts = %d", dispatcher.Dispatches)
	}
	messages, _ = store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversations[0].ID, Limit: 10,
	})
	runs, _ = store.ListAgentRuns(ctx, AgentRunFilter{Scope: endpoint.Scope, Kind: RunKindConversation, Limit: 10})
	if len(messages) != 1 || len(runs) != 1 {
		t.Fatalf("recovery duplicated canonical facts: messages=%d Runs=%d", len(messages), len(runs))
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: endpoint.Scope, ConversationID: conversations[0].ID, Limit: 10,
	})
	if err != nil || len(deliveries) != 0 {
		t.Fatalf("inbox worker emitted presentation-specific progress = %#v, %v", deliveries, err)
	}
	conversationMapping, err := store.GetExternalConversationMapping(
		ctx, endpoint.Scope, endpoint.ID, item.Event.ExternalConversationID, "",
	)
	threadMapping, threadErr := store.GetExternalConversationMapping(
		ctx, endpoint.Scope, endpoint.ID, item.Event.ExternalConversationID, item.Event.ExternalThreadID,
	)
	messageMapping, messageErr := store.GetExternalMessageMapping(
		ctx, endpoint.Scope, endpoint.ID, ExternalMessageInbound, item.Event.ExternalMessageID,
	)
	if err != nil || threadErr != nil || messageErr != nil || conversationMapping == nil || threadMapping == nil ||
		threadMapping.ConversationID != conversations[0].ID || threadMapping.ThreadRootMessageID != messages[0].ID ||
		messageMapping == nil || messageMapping.ChannelMessageID != messages[0].ID {
		t.Fatalf("durable mappings = conversation %#v, thread %#v, message %#v, errors %v/%v/%v",
			conversationMapping, threadMapping, messageMapping, err, threadErr, messageErr)
	}
}

func TestCanonicalExternalConversationDispatcherRefusesRunbookHandler(t *testing.T) {
	handler := ExternalConversationHandler{Kind: "runbook", ID: "support"}
	if err := handler.Validate(ObjectiveOwner{Type: OwnerTypeAgent, ID: "support"}); !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("Runbook handler validated: %v", err)
	}
	result, err := externalconversation.NewCanonicalExternalConversationDispatcher(nil).DispatchExternalConversation(t.Context(), externalconversation.ExternalConversationDispatchRequest{
		Endpoint: &ExternalConversationEndpoint{Handler: handler}, Conversation: &Conversation{ID: "conversation"},
		Message: &ChannelMessage{ID: "message"}, IdempotencyKey: "dispatch-key",
	})
	if result != nil || !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("Runbook handler dispatched: %#v, %v", result, err)
	}
}
