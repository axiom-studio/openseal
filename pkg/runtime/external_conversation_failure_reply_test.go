package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func externalConversationFailureReplyFixture(t *testing.T, provider string, status AgentRunStatus) (*MemoryStore, *skill.Catalog, *ExternalConversationEndpoint, *ExternalConversationInboxItem) {
	t.Helper()
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, provider, []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator,
	})
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "External failure",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "failure-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"},
		Intent: MessageIntentQuestion, Content: "Please help",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "failure-question",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Respond", Source: RunSourceEvent,
		IdempotencyKey: "failure-run", Actor: ActivityActor{Type: "service", ID: "test"},
		Visibility: ActivityVisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, endpoint.Scope, created.Run.ID, RunTransitionRequest{
		ExpectedRevision: created.Run.Revision, Status: AgentRunStatusRunning,
		Summary: "Started", Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	finished, _, err := activity.TransitionRun(ctx, endpoint.Scope, running.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: status,
		Summary: "Stopped", Actor: ActivityActor{Type: "worker", ID: "test"},
		Error:  "unauthorized action_1: bot_token=private-secret",
		Output: map[string]interface{}{"reply": "Internal tool action_1 failed with private-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &ExternalConversationInboxItem{
		ID: "failure-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "failure-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "origin-room", ExternalMessageID: "origin-message",
			ExternalParticipantID: "sender", Text: "Please help", OrderingKey: "origin-thread", OccurredAt: now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: inbound.Message.ID, RunID: finished.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
		t.Fatal(err)
	}
	return store, catalog, endpoint, item
}

func TestExternalConversationFailureReplyIsSafeScopedAndIdempotent(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, provider, AgentRunStatusFailed)
			worker, err := NewExternalConversationReplyWorker(store, catalog)
			if err != nil {
				t.Fatal(err)
			}
			first, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
			if err != nil || len(first) != 1 || first[0].ExternalConversationID != item.Event.ExternalConversationID ||
				first[0].ExternalThreadID != item.Event.ExternalMessageID || first[0].EndpointID != item.EndpointID {
				t.Fatalf("failure projection = %#v, %v", first, err)
			}
			second, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
			if err != nil || len(second) != 0 {
				t.Fatalf("a projected failure must not be revisited: %#v, %v", second, err)
			}
			if replayed := reprojectAppliedExternalInbox(t, worker, store, endpoint.Scope); len(replayed) != 1 || replayed[0].ID != first[0].ID {
				t.Fatalf("failure replay = %#v", replayed)
			}
			messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: item.ConversationID, Limit: 10})
			if err != nil || len(messages) != 2 {
				t.Fatalf("canonical failure messages = %#v, %v", messages, err)
			}
			reply := messages[1]
			if reply.Content != TerminalFailureReply("execution_failed") || reply.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.DeploymentID}) ||
				reply.Scope != item.Scope || reply.ConversationID != item.ConversationID || reply.ThreadRootID != item.ChannelMessageID ||
				reply.ReplyToMessageID != item.ChannelMessageID || reply.ResolvesMessageID != item.ChannelMessageID ||
				len(reply.References) != 1 || reply.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: item.RunID}) ||
				first[0].ChannelMessageID != reply.ID ||
				strings.Contains(reply.Content, "action_1") || strings.Contains(reply.Content, "private-secret") || strings.Contains(reply.Content, "bot_token") {
				t.Fatalf("unsafe or uncorrelated failure reply: %#v", reply)
			}
			deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, ConversationID: item.ConversationID, Limit: 10})
			if err != nil || len(deliveries) != 2 {
				t.Fatalf("failure outbox = %#v, %v", deliveries, err)
			}
			var status *ExternalConversationDelivery
			for _, delivery := range deliveries {
				if delivery.Operation == capability.ConversationDeliveryTypingIndicator {
					status = delivery
				}
			}
			if status == nil || status.Parameters["status"] != "" || status.CreatedAt.Before(first[0].CreatedAt) || status.OrderingKey != first[0].OrderingKey {
				t.Fatalf("status does not follow failure reply: %#v", status)
			}
		})
	}
}

func TestExternalConversationFailureReplySkipsInactiveEndpointAndCanceledRun(t *testing.T) {
	for _, scenario := range []string{"paused", "canceled", "revision-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			status := AgentRunStatusFailed
			if scenario == "canceled" {
				status = AgentRunStatusCanceled
			}
			store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, "slack", status)
			if scenario != "canceled" {
				changed := cloneExternalConversationEndpoint(endpoint)
				changed.Revision++
				changed.UpdatedAt = changed.UpdatedAt.Add(time.Second)
				if scenario == "paused" {
					changed.Status = ExternalConversationEndpointPaused
				}
				if err := store.UpdateExternalConversationEndpoint(ctx, changed, endpoint.Revision); err != nil {
					t.Fatal(err)
				}
			}
			worker, _ := NewExternalConversationReplyWorker(store, catalog)
			deliveries, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
			if scenario == "revision-conflict" && !errors.Is(err, ErrExternalConversationConflict) {
				t.Fatalf("revision conflict = %v", err)
			}
			if len(deliveries) != 0 || (scenario != "revision-conflict" && err != nil) {
				t.Fatalf("suppressed failure projection = %#v, %v", deliveries, err)
			}
			messages, _ := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: item.ConversationID, Limit: 10})
			if len(messages) != 1 {
				t.Fatalf("suppressed run posted a reply: %#v", messages)
			}
		})
	}
}

func TestExternalConversationFailureReplySupersessionIsThreadLocal(t *testing.T) {
	for _, scenario := range []string{"same-thread-question", "same-thread-answer", "other-thread-question", "other-thread-answer"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, "slack", AgentRunStatusFailed)
			conversations := NewConversationService(store)
			current, _ := conversations.GetConversation(ctx, endpoint.Scope, item.ConversationID)
			sender := ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}
			intent := MessageIntentQuestion
			if strings.HasSuffix(scenario, "answer") {
				sender = ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.DeploymentID}
				intent = MessageIntentAnswer
			}
			replyTo := ""
			if strings.HasPrefix(scenario, "same-thread") {
				replyTo = item.ChannelMessageID
			}
			_, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: endpoint.Scope, ConversationID: item.ConversationID, ExpectedRevision: current.Revision,
				Sender: sender, Intent: intent, Content: "Follow-up",
				Audience:         ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: replyTo, IdempotencyKey: "follow-up",
			})
			if err != nil {
				t.Fatal(err)
			}
			worker, _ := NewExternalConversationReplyWorker(store, catalog)
			deliveries, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
			want := 1
			if strings.HasPrefix(scenario, "same-thread") {
				want = 0
			}
			if err != nil || len(deliveries) != want {
				t.Fatalf("%s deliveries = %#v, %v", scenario, deliveries, err)
			}
		})
	}
}

func TestExternalConversationFailedStatusWaitsForReplyProjection(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, "slack", AgentRunStatusFailed)
	worker, err := NewRunProgressAcknowledgementWorker(store, catalog, RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
		t.Fatal("failed status should not trigger a model call")
		return "", nil
	}), RunProgressAcknowledgementWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	status, err := worker.projectThreadStatus(ctx, item)
	if err != nil || status != nil {
		t.Fatalf("failed run cleared status before its notice: %#v, %v", status, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 0 {
		t.Fatalf("failed status was queued independently: %#v, %v", deliveries, err)
	}
}

func TestExternalConversationFailureReplySkipsLegacyTerminalProjection(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, "slack", AgentRunStatusFailed)
	legacy, err := NewExternalConversationTransportService(store, catalog).Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: item.EndpointID, Operation: capability.ConversationDeliveryTypingIndicator,
		ConversationID: item.ConversationID, ChannelMessageID: item.ChannelMessageID,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: item.Event.ExternalMessageID,
		Parameters:     map[string]interface{}{"state": "active", "status": ""},
		IdempotencyKey: "run-thread-status:" + item.ID + ":active:",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := NewExternalConversationReplyWorker(store, catalog)
	for range 2 {
		projected, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
		if err != nil || len(projected) != 0 {
			t.Fatalf("historical failure was replayed after upgrade: %#v, %v", projected, err)
		}
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: item.ConversationID, Limit: 10})
	if err != nil || len(messages) != 1 {
		t.Fatalf("legacy failure posted a new message: %#v, %v", messages, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, ConversationID: item.ConversationID, Limit: 10})
	if err != nil || len(deliveries) != 1 || deliveries[0].ID != legacy.Delivery.ID {
		t.Fatalf("legacy terminal projection changed: %#v, %v", deliveries, err)
	}
}
