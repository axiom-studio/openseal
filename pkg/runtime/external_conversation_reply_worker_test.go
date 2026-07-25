package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestExternalConversationReplyWorkerProjectsRunbookReplyExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Slack thread",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "reply-worker-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "U123"},
		Intent: MessageIntentQuestion, Content: "Can you help?",
		Audience:         ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "reply-worker-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Respond", Source: RunSourceEvent,
		IdempotencyKey: "reply-worker-run",
		Actor:          ActivityActor{Type: "service", ID: "external-conversation"}, Visibility: ActivityVisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, endpoint.Scope, runResult.Run.ID, RunTransitionRequest{
		ExpectedRevision: runResult.Run.Revision, Status: AgentRunStatusRunning,
		Summary: "Started", Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, _, err := activity.TransitionRun(ctx, endpoint.Scope, running.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted,
		Summary: "Completed", Actor: ActivityActor{Type: "worker", ID: "test"},
		Output: map[string]interface{}{"reply": "Yes — I can help with that."},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &ExternalConversationInboxItem{
		ID: "reply-worker-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "Ev-reply", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.001",
			ExternalParticipantID: "U123", Text: "Can you help?", OrderingKey: "C123:171.001", OccurredAt: now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: inbound.Message.ID, RunID: completed.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, replayed, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil || replayed {
		t.Fatalf("store applied inbox replayed=%t err=%v", replayed, err)
	}
	worker, err := NewExternalConversationReplyWorker(store, catalog)
	if err != nil {
		t.Fatal(err)
	}
	first, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(first) != 1 || first[0].Status != ExternalConversationDeliveryPending ||
		first[0].ExternalThreadID != item.Event.ExternalMessageID {
		t.Fatalf("first projection = %#v, %v", first, err)
	}
	second, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("replayed projection = %#v, %v", second, err)
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10,
	})
	if err != nil || len(messages) != 2 || messages[1].Content != "Yes — I can help with that." ||
		messages[1].ReplyToMessageID != inbound.Message.ID {
		t.Fatalf("canonical messages = %#v, %v", messages, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10,
	})
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("durable deliveries = %#v, %v", deliveries, err)
	}
}

func TestExternalConversationReplyWorkerUsesExistingDirectHandlerMessage(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "webchat")
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Web chat",
		IdempotencyKey: "direct-reply-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "visitor"},
		Intent: MessageIntentQuestion, Content: "Hello",
		Audience:         ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "direct-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Respond", Source: RunSourceEvent,
		IdempotencyKey: "direct-reply-run",
		Actor:          ActivityActor{Type: "service", ID: "external-conversation"}, Visibility: ActivityVisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := conversations.GetConversation(ctx, endpoint.Scope, conversation.ID)
	reply, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.DeploymentID},
		Intent: MessageIntentAnswer, Content: "Hello there",
		Audience:         ConversationAudience{Kind: ConversationAudienceChannel},
		ReplyToMessageID: inbound.Message.ID,
		References:       []ConversationReference{{Kind: ConversationReferenceRun, ID: runResult.Run.ID}},
		IdempotencyKey:   "direct-handler-response",
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, _ := activity.TransitionRun(ctx, endpoint.Scope, runResult.Run.ID, RunTransitionRequest{
		ExpectedRevision: runResult.Run.Revision, Status: AgentRunStatusRunning,
		Summary: "Started", Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	completed, _, err := activity.TransitionRun(ctx, endpoint.Scope, running.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted,
		Summary: "Completed", Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &ExternalConversationInboxItem{
		ID: "direct-reply-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "web-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "room", ExternalMessageID: "message-one",
			ExternalParticipantID: "visitor", Text: "Hello", OrderingKey: "room:message-one", OccurredAt: now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: inbound.Message.ID, RunID: completed.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
		t.Fatal(err)
	}
	worker, _ := NewExternalConversationReplyWorker(store, catalog)
	deliveries, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(deliveries) != 1 || deliveries[0].ChannelMessageID != reply.Message.ID {
		t.Fatalf("direct delivery = %#v, %v", deliveries, err)
	}
	messages, _ := store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10,
	})
	if len(messages) != 2 {
		t.Fatalf("direct handler response was duplicated: %#v", messages)
	}
}
