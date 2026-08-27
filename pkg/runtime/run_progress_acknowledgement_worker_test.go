package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestRunProgressAcknowledgementWorkerProjectsOptInRunProgressExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "webchat")
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Support thread",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "text-ack-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "speaker"},
		Intent: MessageIntentQuestion, Content: "Hey Cody, can you check this?",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "text-ack-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Kind: RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Respond", Source: RunSourceChat,
		IdempotencyKey: "text-ack-run", Actor: ActivityActor{Type: "service", ID: "channel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := NewRunActivityService(store, store).TransitionRun(ctx, endpoint.Scope, runResult.Run.ID, RunTransitionRequest{
		ExpectedRevision: runResult.Run.Revision, Status: AgentRunStatusRunning,
		Summary: "Run claimed", EventType: "run.claimed", Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := &ExternalConversationInboxItem{
		ID: "text-ack-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "text-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "meeting", ExternalThreadID: "speaker-turn", ExternalMessageID: "utterance-1",
			ExternalParticipantID: "speaker", Text: "Hey Cody, can you check this?",
			OrderingKey: "meeting:utterance-1", OccurredAt: now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: now,
		ConversationID: conversation.ID, ChannelMessageID: inbound.Message.ID, RunID: running.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: now,
	}
	if _, replayed, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil || replayed {
		t.Fatalf("store inbox replayed=%t err=%v", replayed, err)
	}
	worker, err := NewRunProgressAcknowledgementWorker(store, catalog, RunProgressAcknowledgementWorkerConfig{
		MinimumRunAge: time.Nanosecond, MinimumInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now.Add(time.Minute) }
	first, err := worker.ProcessScope(ctx, endpoint.Scope)
	if err != nil || len(first) != 1 || first[0].Correlation == nil ||
		first[0].Correlation.Kind != runProgressAcknowledgementCorrelationKind ||
		first[0].Correlation.ID != running.ID || first[0].Correlation.Phase != "starting" ||
		first[0].ExternalThreadID != "speaker-turn" {
		t.Fatalf("first acknowledgements = %#v, %v", first, err)
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10,
	})
	if err != nil || len(messages) != 2 || messages[1].Intent != MessageIntentAcknowledgment ||
		messages[1].Content != "Starting work" || messages[1].ResolvesMessageID != "" {
		t.Fatalf("acknowledgement messages = %#v, %v", messages, err)
	}
	second, err := worker.ProcessScope(ctx, endpoint.Scope)
	if err != nil || len(second) != 0 {
		t.Fatalf("duplicate acknowledgement = %#v, %v", second, err)
	}
}

func TestProjectRunProgressAcknowledgementUsesBoundedEvidenceBackedPhrases(t *testing.T) {
	run := &AgentRun{ID: "run-one", Status: AgentRunStatusRunning, Revision: 3}
	cases := []struct {
		name  string
		event *ActivityEvent
		text  string
	}{
		{name: "generic", text: "Working on it"},
		{name: "tool", event: &ActivityEvent{ID: "event-tool", EventType: "action.proposed", Payload: map[string]interface{}{"skillId": "skill-github"}}, text: "Using GitHub"},
		{name: "retry", event: &ActivityEvent{ID: "event-retry", EventType: "action.retry_scheduled"}, text: "Trying that again"},
		{name: "approval", event: &ActivityEvent{ID: "event-approval", EventType: "action.approval_requested"}, text: "Waiting for approval"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			acknowledgement, ok := projectRunProgressAcknowledgement(run, test.event)
			if !ok || acknowledgement.Text != test.text {
				t.Fatalf("acknowledgement = %#v, %t", acknowledgement, ok)
			}
			words := len(strings.Fields(acknowledgement.Text))
			if words < 2 || words > 3 {
				t.Fatalf("acknowledgement has %d words: %q", words, acknowledgement.Text)
			}
		})
	}
}
