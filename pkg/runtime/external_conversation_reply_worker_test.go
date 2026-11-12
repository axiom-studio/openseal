package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func TestExternalConversationReplyWorkerProjectsRunbookReplyExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator,
	})
	conversations := runtimeconversation.NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Slack thread",
		Origin:         &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "reply-worker-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "U123"},
		Intent: models.MessageIntentQuestion, Content: "Can you help?",
		Audience:         models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "reply-worker-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: models.RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Respond", Source: models.RunSourceEvent,
		IdempotencyKey: "reply-worker-run",
		Actor:          models.ActivityActor{Type: "service", ID: "external-conversation"}, Visibility: models.ActivityVisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, endpoint.Scope, runResult.Run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: runResult.Run.Revision, Status: models.AgentRunStatusRunning,
		Summary: "Started", Actor: models.ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, _, err := activity.TransitionRun(ctx, endpoint.Scope, running.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusCompleted,
		Summary: "Completed", Actor: models.ActivityActor{Type: "worker", ID: "test"},
		Output: map[string]interface{}{"reply": "Yes — I can help with that."},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := conversations.GetConversation(ctx, endpoint.Scope, conversation.ID)
	_, err = conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: endpoint.DeploymentID},
		Intent: models.MessageIntentAcknowledgment, Content: "Working on it",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: inbound.Message.ID,
		References:     []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: completed.ID}},
		IdempotencyKey: "reply-worker-progress-ack",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &models.ExternalConversationInboxItem{
		ID: "reply-worker-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: models.NormalizedExternalConversationEvent{
			ID: "Ev-reply", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.001",
			ExternalParticipantID: "U123", Text: "Can you help?", OrderingKey: "C123:171.001", OccurredAt: now,
		},
		Status: models.ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
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
	if err != nil || len(first) != 1 || first[0].Status != models.ExternalConversationDeliveryPending ||
		first[0].ExternalThreadID != item.Event.ExternalMessageID {
		t.Fatalf("first projection = %#v, %v", first, err)
	}
	second, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(second) != 0 {
		t.Fatalf("a projected inbox item must not be revisited: %#v, %v", second, err)
	}
	if replayed := reprojectAppliedExternalInbox(t, worker, store, endpoint.Scope); len(replayed) != 1 || replayed[0].ID != first[0].ID {
		t.Fatalf("replayed projection = %#v", replayed)
	}
	messages, err := store.ListChannelMessages(ctx, models.ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10,
	})
	if err != nil || len(messages) != 3 || messages[1].Content != "Working on it" ||
		messages[2].Content != "Yes — I can help with that." || messages[2].ReplyToMessageID != inbound.Message.ID {
		t.Fatalf("canonical messages = %#v, %v", messages, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, models.ExternalConversationDeliveryFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10,
	})
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("durable deliveries = %#v, %v", deliveries, err)
	}
	operations := map[capability.ConversationDeliveryOperation]*models.ExternalConversationDelivery{}
	for _, delivery := range deliveries {
		operations[delivery.Operation] = delivery
	}
	if operations[capability.ConversationDeliveryMessageSend] == nil ||
		operations[capability.ConversationDeliveryTypingIndicator] == nil ||
		operations[capability.ConversationDeliveryTypingIndicator].Parameters["status"] != "" {
		t.Fatalf("durable delivery operations = %#v", operations)
	}
	retired := models.CloneExternalConversationEndpoint(endpoint)
	retired.Status = models.ExternalConversationEndpointRetired
	retired.Revision++
	retired.UpdatedAt = now.Add(time.Second)
	retired.RetiredAt = &retired.UpdatedAt
	if err := store.UpdateExternalConversationEndpoint(ctx, retired, endpoint.Revision); err != nil {
		t.Fatal(err)
	}
	afterRetirement, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(afterRetirement) != 0 {
		t.Fatalf("projection after endpoint retirement = %#v, %v", afterRetirement, err)
	}
}

func TestExternalConversationReplyWorkerUsesExistingDirectHandlerMessage(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "webchat")
	conversations := runtimeconversation.NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Web chat",
		IdempotencyKey: "direct-reply-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "visitor"},
		Intent: models.MessageIntentQuestion, Content: "Hello",
		Audience:         models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "direct-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: models.RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Respond", Source: models.RunSourceEvent,
		IdempotencyKey: "direct-reply-run",
		Actor:          models.ActivityActor{Type: "service", ID: "external-conversation"}, Visibility: models.ActivityVisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := conversations.GetConversation(ctx, endpoint.Scope, conversation.ID)
	reply, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: endpoint.DeploymentID},
		Intent: models.MessageIntentAnswer, Content: "Hello there",
		Audience:         models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		ReplyToMessageID: inbound.Message.ID,
		References:       []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: runResult.Run.ID}},
		IdempotencyKey:   "direct-handler-response",
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	running, _, _ := activity.TransitionRun(ctx, endpoint.Scope, runResult.Run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: runResult.Run.Revision, Status: models.AgentRunStatusRunning,
		Summary: "Started", Actor: models.ActivityActor{Type: "worker", ID: "test"},
	})
	completed, _, err := activity.TransitionRun(ctx, endpoint.Scope, running.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusCompleted,
		Summary: "Completed", Actor: models.ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &models.ExternalConversationInboxItem{
		ID: "direct-reply-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: models.NormalizedExternalConversationEvent{
			ID: "web-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "room", ExternalMessageID: "message-one",
			ExternalParticipantID: "visitor", Text: "Hello", OrderingKey: "room:message-one", OccurredAt: now,
		},
		Status: models.ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
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
	messages, _ := store.ListChannelMessages(ctx, models.ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10,
	})
	if len(messages) != 2 {
		t.Fatalf("direct handler response was duplicated: %#v", messages)
	}
}

// reprojectAppliedExternalInbox projects every applied item directly,
// bypassing the worker's terminal reply state, to prove projection itself
// stays idempotent.
func reprojectAppliedExternalInbox(t *testing.T, worker *ExternalConversationReplyWorker, store models.ExternalConversationStore, scope models.Scope) []*models.ExternalConversationDelivery {
	t.Helper()
	items, err := store.ListExternalConversationInbox(t.Context(), models.ExternalConversationInboxFilter{
		Scope: scope, Statuses: []models.ExternalConversationInboxStatus{models.ExternalConversationInboxApplied}, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	var deliveries []*models.ExternalConversationDelivery
	for _, item := range items {
		projected, _, err := worker.project(t.Context(), item)
		if err != nil {
			t.Fatal(err)
		}
		deliveries = append(deliveries, projected...)
	}
	return deliveries
}
