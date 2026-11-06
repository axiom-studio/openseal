package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// teamChannelFixture is a Team-owned provider channel whose message was
// routed to two members, each with its own member Run.
type teamChannelFixture struct {
	t            *testing.T
	store        *MemoryStore
	catalog      *skill.Catalog
	endpoint     *ExternalConversationEndpoint
	conversation *Conversation
	inbound      *ChannelMessage
	routing      *AgentRun
	members      map[string]*AgentRun
	now          time.Time
}

var teamChannelMembers = []string{"agent-a", "agent-b"}

func newTeamChannelFixture(t *testing.T, operations ...capability.ConversationDeliveryOperation) *teamChannelFixture {
	t.Helper()
	ctx := t.Context()
	store := NewMemoryStore()
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "support-team"}
	catalog, endpoint := externalConversationEndpointFixtureOwnedBy(t, ctx, store, "slack", operations, owner)
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: endpoint.Scope, Owner: owner, Title: "Support channel",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "team-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "speaker"},
		Intent: MessageIntentQuestion, Content: "Is the deploy healthy?",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "team-inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &teamChannelFixture{t: t, store: store, catalog: catalog, endpoint: endpoint, conversation: conversation, inbound: inbound.Message, members: map[string]*AgentRun{}, now: time.Now().UTC()}
	commands := NewRunCommandService(store)
	routing, err := commands.CreateAgentRun(ctx, conversationAgentRunRequest(conversation, inbound.Message))
	if err != nil {
		t.Fatal(err)
	}
	f.routing = f.transition(routing.Run, AgentRunStatusRunning, nil)
	responders := make([]interface{}, 0, len(teamChannelMembers))
	for _, id := range teamChannelMembers {
		responder := ConversationResponder{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: id}}
		created, err := commands.CreateAgentRun(ctx, teamMemberRunRequest(f.routing, conversation, inbound.Message, responder, teamMemberRunKey(f.routing, id)))
		if err != nil {
			t.Fatal(err)
		}
		f.members[id] = f.transition(created.Run, AgentRunStatusRunning, nil)
		responders = append(responders, created.Run.ID)
	}
	f.routing = f.transition(f.routing, AgentRunStatusCompleted, map[string]interface{}{"responderRunIds": responders})
	item := &ExternalConversationInboxItem{
		ID: "team-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID,
		EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "team-event", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "conversation", ExternalThreadID: "thread", ExternalMessageID: "message",
			ExternalParticipantID: "speaker", Text: "is the deploy healthy?", OrderingKey: "conversation:message", OccurredAt: f.now,
		},
		Status: ExternalConversationInboxApplied, MaximumAttempts: 8, AvailableAt: f.now,
		ConversationID: conversation.ID, ChannelMessageID: inbound.Message.ID, RunID: f.routing.ID,
		Revision: 1, CreatedAt: f.now, UpdatedAt: f.now, AppliedAt: f.now,
	}
	if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *teamChannelFixture) transition(run *AgentRun, status AgentRunStatus, output map[string]interface{}) *AgentRun {
	f.t.Helper()
	next, _, err := NewRunActivityService(f.store, f.store).TransitionRun(f.t.Context(), run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, Summary: string(status), Output: output,
		Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return next
}

// answer posts the member's own answer, as its turn does, and completes it.
func (f *teamChannelFixture) answer(member, text string) *ChannelMessage {
	f.t.Helper()
	ctx := f.t.Context()
	run := f.members[member]
	conversations := NewConversationService(f.store)
	current, _ := conversations.GetConversation(ctx, f.endpoint.Scope, f.conversation.ID)
	posted, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: f.endpoint.Scope, ConversationID: f.conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: member},
		Intent: MessageIntentAnswer, Content: text, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		ReplyToMessageID: f.inbound.ID, References: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}},
		IdempotencyKey: conversationTaskFinalResponseKey(run),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.members[member] = f.transition(run, AgentRunStatusCompleted, nil)
	return posted.Message
}

func (f *teamChannelFixture) deliveries(operation capability.ConversationDeliveryOperation) []*ExternalConversationDelivery {
	f.t.Helper()
	all, err := f.store.ListExternalConversationDeliveries(f.t.Context(), ExternalConversationDeliveryFilter{Scope: f.endpoint.Scope, EndpointID: f.endpoint.ID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	var result []*ExternalConversationDelivery
	for _, delivery := range all {
		if delivery.Operation == operation {
			result = append(result, delivery)
		}
	}
	return result
}

func TestExternalTeamChannelDeliversEachMemberAnswer(t *testing.T) {
	f := newTeamChannelFixture(t, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator)
	worker, err := NewExternalConversationReplyWorker(f.store, f.catalog)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if deliveries, err := worker.ProcessScope(ctx, f.endpoint.Scope, 10); err != nil || len(deliveries) != 0 {
		t.Fatalf("members still answering: %#v, %v", deliveries, err)
	}

	first := f.answer("agent-a", "The deploy is healthy.")
	deliveries, err := worker.ProcessScope(ctx, f.endpoint.Scope, 10)
	if err != nil || len(deliveries) != 1 || deliveries[0].ChannelMessageID != first.ID || deliveries[0].ExternalThreadID != "thread" {
		t.Fatalf("first member answer = %#v, %v", deliveries, err)
	}
	if status := f.deliveries(capability.ConversationDeliveryTypingIndicator); len(status) != 0 {
		t.Fatalf("thread went idle while agent-b still works: %#v", status)
	}

	// agent-b's answer is its typed reply output; the worker posts it as agent-b.
	f.members["agent-b"] = f.transition(f.members["agent-b"], AgentRunStatusCompleted, map[string]interface{}{"reply": "Latency is normal too."})
	deliveries, err = worker.ProcessScope(ctx, f.endpoint.Scope, 10)
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("both member answers = %#v, %v", deliveries, err)
	}
	second, err := f.store.GetChannelMessage(ctx, f.endpoint.Scope, f.conversation.ID, deliveries[1].ChannelMessageID)
	if err != nil || second.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-b"}) ||
		second.Content != "Latency is normal too." || second.ReplyToMessageID != f.inbound.ID {
		t.Fatalf("second member answer = %#v, %v", second, err)
	}
	if deliveries[0].ID == deliveries[1].ID || deliveries[0].IdempotencyKey == deliveries[1].IdempotencyKey {
		t.Fatalf("member answers share a delivery: %#v", deliveries)
	}
	if status := f.deliveries(capability.ConversationDeliveryTypingIndicator); len(status) != 1 || status[0].Parameters["status"] != "" {
		t.Fatalf("thread status after the last answer = %#v", status)
	}
	if again, err := worker.ProcessScope(ctx, f.endpoint.Scope, 10); err != nil || len(again) != 0 {
		t.Fatalf("resolved item projected again: %#v, %v", again, err)
	}
	if sends := f.deliveries(capability.ConversationDeliveryMessageSend); len(sends) != 2 {
		t.Fatalf("answers were duplicated: %#v", sends)
	}
}

func TestExternalTeamChannelRejectsUnverifiedMemberRun(t *testing.T) {
	f := newTeamChannelFixture(t, capability.ConversationDeliveryMessageSend)
	ctx := t.Context()
	// A member Run that the routing Run did not start, listed as a responder.
	forged, err := NewRunCommandService(f.store).CreateAgentRun(ctx, teamMemberRunRequest(f.routing, f.conversation, f.inbound,
		ConversationResponder{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-c"}}, "forged-member"))
	if err != nil {
		t.Fatal(err)
	}
	f.routing.Output["responderRunIds"] = []interface{}{forged.Run.ID}
	items, err := f.store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{Scope: f.endpoint.Scope, Limit: 1})
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	if _, err := externalItemReplies(ctx, routingOverride{f.store, f.routing}, items[0]); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("unverified member Run = %v", err)
	}
}

// routingOverride serves a routing Run whose output a test changed.
type routingOverride struct {
	*MemoryStore
	routing *AgentRun
}

func (s routingOverride) GetAgentRun(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
	if id == s.routing.ID {
		return s.routing, nil
	}
	return s.MemoryStore.GetAgentRun(ctx, scope, id)
}

func TestExternalTeamChannelForwardsMemberProgress(t *testing.T) {
	f := newTeamChannelFixture(t, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate, capability.ConversationDeliveryTypingIndicator)
	worker, err := NewRunProgressAcknowledgementWorker(f.store, f.catalog, RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
		t.Fatal("commentary must not render progress")
		return "", nil
	}), RunProgressAcknowledgementWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	activity := NewRunActivityService(f.store, f.store)
	if _, err := activity.AppendActivity(ctx, &ActivityEvent{
		Scope: f.endpoint.Scope, RunID: f.members["agent-a"].ID, EventType: "turn.commentary", Summary: "Checking the health endpoint.",
		Actor: ActivityActor{Type: "worker", ID: "test"}, Visibility: ActivityVisibilityScope,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := activity.AppendActivity(ctx, &ActivityEvent{
		Scope: f.endpoint.Scope, RunID: f.members["agent-b"].ID, EventType: "turn.answer_preview", Summary: "Reply preview updated",
		Actor: ActivityActor{Type: "worker", ID: "test"}, Visibility: ActivityVisibilityScope,
		Payload: map[string]interface{}{"attemptId": "attempt", "sequence": 1, "text": "Latency has been normal all morning", "reset": false},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessCommentaryScope(ctx, f.endpoint.Scope); err != nil {
		t.Fatal(err)
	}
	var commentary, preview *ExternalConversationDelivery
	for _, delivery := range f.deliveries(capability.ConversationDeliveryMessageSend) {
		switch delivery.Correlation.Kind {
		case "run_commentary":
			commentary = delivery
		case externalAnswerStreamCorrelationKind:
			preview = delivery
		}
	}
	if commentary == nil || commentary.Correlation.ID != f.members["agent-a"].ID {
		t.Fatalf("member commentary = %#v", commentary)
	}
	message, err := f.store.GetChannelMessage(ctx, f.endpoint.Scope, f.conversation.ID, commentary.ChannelMessageID)
	if err != nil || message.Sender.ID != "agent-a" {
		t.Fatalf("commentary sender = %#v, %v", message, err)
	}
	if preview == nil || preview.Parameters[externalAnswerStreamAgentParameter] != "agent-b" ||
		preview.Correlation.ID != "team-inbox:"+f.members["agent-b"].ID {
		t.Fatalf("member answer stream = %#v", preview)
	}
	status := f.deliveries(capability.ConversationDeliveryTypingIndicator)
	if len(status) != 2 {
		t.Fatalf("thread status per working member = %#v", status)
	}
	for _, delivery := range status {
		if delivery.Parameters["state"] != "processing" {
			t.Fatalf("thread status = %#v", delivery)
		}
	}
}
