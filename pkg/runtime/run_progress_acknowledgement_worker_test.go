package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type runProgressWorkerFixture struct {
	store        *MemoryStore
	catalog      *skill.Catalog
	endpoint     *ExternalConversationEndpoint
	conversation *Conversation
	inbound      *ChannelMessage
	run          *AgentRun
	now          time.Time
}

func newRunProgressWorkerFixture(t *testing.T, provider string, operations []capability.ConversationDeliveryOperation) runProgressWorkerFixture {
	t.Helper()
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, provider, operations)
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Support thread",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "progress-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "speaker"},
		Intent: MessageIntentQuestion, Content: "Can you check this?",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "progress-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Check the deployment", Source: RunSourceChat,
		IdempotencyKey: "progress-run", Actor: ActivityActor{Type: "service", ID: "channel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := NewRunActivityService(store, store).TransitionRun(ctx, endpoint.Scope, runResult.Run.ID, RunTransitionRequest{
		ExpectedRevision: runResult.Run.Revision, Status: AgentRunStatusRunning,
		Summary: "Inspecting the current deployment", EventType: "run.observed", Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &ExternalConversationInboxItem{
		ID: "progress-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "progress-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "conversation", ExternalThreadID: "thread", ExternalMessageID: "message",
			ExternalParticipantID: "speaker", Text: "Can you check this?", OrderingKey: "conversation:message", OccurredAt: now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: inbound.Message.ID, RunID: running.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, replayed, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil || replayed {
		t.Fatalf("store inbox replayed=%t err=%v", replayed, err)
	}
	return runProgressWorkerFixture{store: store, catalog: catalog, endpoint: endpoint, conversation: conversation, inbound: inbound.Message, run: running, now: now}
}

func TestRunProgressAcknowledgementWorkerUsesNativeProgressCapability(t *testing.T) {
	fixture := newRunProgressWorkerFixture(t, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator,
	})
	var rendered RunProgressAcknowledgementRequest
	renderer := RunProgressAcknowledgementRendererFunc(func(_ context.Context, request RunProgressAcknowledgementRequest) (string, error) {
		rendered = request
		return "Inspecting it, obviously.", nil
	})
	worker, err := NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, renderer, RunProgressAcknowledgementWorkerConfig{
		MinimumRunAge: time.Nanosecond, MinimumInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return fixture.now.Add(time.Minute) }
	deliveries, err := worker.ProcessScope(context.Background(), fixture.endpoint.Scope)
	if err != nil || len(deliveries) != 1 || deliveries[0].Operation != capability.ConversationDeliveryTypingIndicator ||
		deliveries[0].Parameters["status"] != "Inspecting it, obviously." || deliveries[0].ExternalThreadID != "thread" ||
		deliveries[0].Correlation == nil || !strings.HasPrefix(deliveries[0].Correlation.Phase, "progress-") {
		t.Fatalf("native progress delivery = %#v, %v", deliveries, err)
	}
	if rendered.Snapshot.Goal != "Check the deployment" || rendered.Snapshot.ActivitySummary != "Inspecting the current deployment" ||
		rendered.MaxSentences != 2 || rendered.MaxCharacters != 100 {
		t.Fatalf("renderer request = %#v", rendered)
	}
	messages, _ := fixture.store.ListChannelMessages(context.Background(), ChannelMessageFilter{
		Scope: fixture.endpoint.Scope, ConversationID: fixture.conversation.ID, Limit: 10,
	})
	if len(messages) != 1 {
		t.Fatalf("native progress created a chat message: %#v", messages)
	}
	second, err := worker.ProcessScope(context.Background(), fixture.endpoint.Scope)
	if err != nil || len(second) != 0 {
		t.Fatalf("duplicate native progress = %#v, %v", second, err)
	}
}

func TestRunProgressAcknowledgementWorkerFallsBackToText(t *testing.T) {
	fixture := newRunProgressWorkerFixture(t, "webchat", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
	renderer := RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
		return "I am checking now.", nil
	})
	worker, err := NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, renderer, RunProgressAcknowledgementWorkerConfig{
		MinimumRunAge: time.Nanosecond, MinimumInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return fixture.now.Add(time.Minute) }
	deliveries, err := worker.ProcessScope(context.Background(), fixture.endpoint.Scope)
	if err != nil || len(deliveries) != 1 || deliveries[0].Operation != capability.ConversationDeliveryMessageSend {
		t.Fatalf("text fallback delivery = %#v, %v", deliveries, err)
	}
	messages, _ := fixture.store.ListChannelMessages(context.Background(), ChannelMessageFilter{
		Scope: fixture.endpoint.Scope, ConversationID: fixture.conversation.ID, Limit: 10,
	})
	if len(messages) != 2 || messages[1].Intent != MessageIntentAcknowledgment || messages[1].Content != "I am checking now." {
		t.Fatalf("text fallback messages = %#v", messages)
	}
}

func TestRenderedRunProgressAcknowledgementIsBounded(t *testing.T) {
	for _, test := range []struct {
		value string
		ok    bool
	}{
		{value: "Checking it now.", ok: true},
		{value: "Checking it now. Patience, please.", ok: true},
		{value: "One. Two. Three.", ok: false},
		{value: strings.Repeat("a", 101), ok: false},
		{value: "line one\nline two", ok: false},
	} {
		_, err := validateRenderedRunProgressAcknowledgement(test.value, 2, 100)
		if (err == nil) != test.ok {
			t.Fatalf("value %q: err=%v", test.value, err)
		}
	}
}
