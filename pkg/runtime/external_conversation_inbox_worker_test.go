package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type crashAfterExternalConversationDispatch struct {
	next       ExternalConversationDispatcher
	crashOnce  bool
	dispatches int
	runID      string
}

func (d *crashAfterExternalConversationDispatch) DispatchExternalConversation(
	ctx context.Context,
	req ExternalConversationDispatchRequest,
) (*ExternalConversationDispatchResult, error) {
	d.dispatches++
	result, err := d.next.DispatchExternalConversation(ctx, req)
	if err != nil {
		return nil, err
	}
	d.runID = result.RunID
	if d.crashOnce {
		d.crashOnce = false
		return nil, errors.New("synthetic crash after durable Run dispatch")
	}
	return result, nil
}

func TestExternalConversationInboxWorkerRecoversWithoutDuplicateMessageOrRun(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
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
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &crashAfterExternalConversationDispatch{
		next: NewCanonicalExternalConversationDispatcher(scheduler, nil), crashOnce: true,
	}
	worker, err := NewExternalConversationInboxWorker(store, dispatcher, ExternalConversationInboxWorkerConfig{
		WorkerID: "inbox-worker", LeaseDuration: time.Minute, BaseRetry: time.Second, MaximumRetry: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }

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
	if err != nil || len(runs) != 1 || runs[0].ID != dispatcher.runID {
		t.Fatalf("Runs after crash = %#v, %v", runs, err)
	}

	worker.now = func() time.Time { return now.Add(time.Second) }
	applied, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil || applied == nil || applied.Status != ExternalConversationInboxApplied ||
		applied.ChannelMessageID != messages[0].ID || applied.RunID != runs[0].ID || applied.Attempt != 2 {
		t.Fatalf("recovered application = %#v, %v", applied, err)
	}
	if dispatcher.dispatches != 2 {
		t.Fatalf("dispatch attempts = %d", dispatcher.dispatches)
	}
	messages, _ = store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversations[0].ID, Limit: 10,
	})
	runs, _ = store.ListAgentRuns(ctx, AgentRunFilter{Scope: endpoint.Scope, Kind: RunKindConversation, Limit: 10})
	if len(messages) != 1 || len(runs) != 1 {
		t.Fatalf("recovery duplicated canonical facts: messages=%d Runs=%d", len(messages), len(runs))
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

type recordingRunbookConversationDispatcher struct {
	handler ExternalConversationHandler
	event   EventEnvelope
	key     string
}

func (d *recordingRunbookConversationDispatcher) DispatchExternalConversationRunbook(
	_ context.Context,
	handler ExternalConversationHandler,
	req ExternalConversationDispatchRequest,
) (*ExternalConversationDispatchResult, error) {
	d.handler, d.event, d.key = handler, req.Event, req.IdempotencyKey
	return &ExternalConversationDispatchResult{RunID: "runbook-run"}, nil
}

func TestCanonicalExternalConversationDispatcherRoutesExactRunbook(t *testing.T) {
	runbooks := &recordingRunbookConversationDispatcher{}
	dispatcher := NewCanonicalExternalConversationDispatcher(nil, runbooks)
	scope := Scope{Kind: "tenant", ID: "one"}
	endpoint := &ExternalConversationEndpoint{
		Handler: ExternalConversationHandler{
			Kind: ExternalConversationHandlerRunbook, ID: "support", Version: "2.4.1", Trigger: "on-message",
			AssignedAgentID: "support-agent",
		},
	}
	conversation := &Conversation{ID: "conversation"}
	message := &ChannelMessage{ID: "message"}
	event := EventEnvelope{
		ID: "event", Scope: scope, Type: externalConversationEventType, Source: "conversation-adapter:webchat",
		OccurredAt: time.Now().UTC(), Payload: map[string]interface{}{},
	}
	result, err := dispatcher.DispatchExternalConversation(t.Context(), ExternalConversationDispatchRequest{
		Endpoint: endpoint, Conversation: conversation, Message: message, Event: event, IdempotencyKey: "dispatch-key",
	})
	if err != nil || result.RunID != "runbook-run" || runbooks.handler != endpoint.Handler ||
		runbooks.event.ID != event.ID || runbooks.key != "dispatch-key" {
		t.Fatalf("Runbook dispatch = %#v, recorder=%#v, %v", result, runbooks, err)
	}
}
