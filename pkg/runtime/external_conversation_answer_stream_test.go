package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// streamChatHost is a provider chat: sends post messages, updates edit them
// and deletes remove them.
type streamChatHost struct {
	messages  map[string]string
	requests  []ExternalConversationDeliveryHostRequest
	rejectAll capability.ConversationDeliveryOperation
	next      int
}

func (h *streamChatHost) LookupExternalConversationDelivery(context.Context, ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryAcknowledgement, error) {
	return &ExternalConversationDeliveryAcknowledgement{Status: ExternalConversationAcknowledgementUnknown}, nil
}

func (h *streamChatHost) DeliverExternalConversation(_ context.Context, request ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryHostResult, error) {
	h.requests = append(h.requests, request)
	if request.Delivery.Operation == h.rejectAll {
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeFailed, ErrorCode: "message_not_found"}, nil
	}
	switch request.Delivery.Operation {
	case capability.ConversationDeliveryMessageSend:
		h.next++
		id := "chat-" + string(rune('0'+h.next))
		h.messages[id] = request.Message.Content
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	case capability.ConversationDeliveryMessageUpdate:
		id, _ := request.Delivery.Parameters["providerMessageId"].(string)
		h.messages[id] = request.Message.Content
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	case capability.ConversationDeliveryMessageDelete:
		id, _ := request.Delivery.Parameters["providerMessageId"].(string)
		delete(h.messages, id)
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	}
	return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: "status"}, nil
}

type answerStreamFixture struct {
	runProgressWorkerFixture
	t        *testing.T
	host     *streamChatHost
	progress *RunProgressAcknowledgementWorker
	replies  *ExternalConversationReplyWorker
	delivery *ExternalConversationDeliveryWorker
	clock    time.Time
	sequence uint64
}

func newAnswerStreamFixture(t *testing.T, operations ...capability.ConversationDeliveryOperation) *answerStreamFixture {
	t.Helper()
	fixture := &answerStreamFixture{
		runProgressWorkerFixture: newRunProgressWorkerFixture(t, "webchat", operations), t: t,
		host: &streamChatHost{messages: map[string]string{}},
	}
	fixture.clock = fixture.now
	var err error
	fixture.progress, err = NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
		return "", errors.New("streaming must not render progress")
	}), RunProgressAcknowledgementWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	fixture.progress.now = func() time.Time { return fixture.clock }
	if fixture.replies, err = NewExternalConversationReplyWorker(fixture.store, fixture.catalog); err != nil {
		t.Fatal(err)
	}
	if fixture.delivery, err = NewExternalConversationDeliveryWorker(fixture.store, fixture.catalog, fixture.host, ExternalConversationDeliveryWorkerConfig{WorkerID: "delivery"}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *answerStreamFixture) preview(text string, reset bool) {
	f.t.Helper()
	f.sequence++
	_, err := NewRunActivityService(f.store, f.store).AppendActivity(f.t.Context(), &ActivityEvent{
		Scope: f.endpoint.Scope, RunID: f.run.ID, EventType: "turn.answer_preview", Summary: "Reply preview updated",
		Actor: ActivityActor{Type: "worker", ID: "test"}, Visibility: ActivityVisibilityScope,
		Payload: map[string]interface{}{"attemptId": "attempt", "sequence": f.sequence, "text": text, "reset": reset},
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *answerStreamFixture) stream() []*ExternalConversationDelivery {
	f.t.Helper()
	deliveries, err := f.progress.ProcessCommentaryScope(f.t.Context(), f.endpoint.Scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return deliveries
}

func (f *answerStreamFixture) deliverAll() {
	f.t.Helper()
	for range 20 {
		delivery, err := f.delivery.ProcessOne(f.t.Context(), f.endpoint.Scope)
		if err != nil {
			f.t.Fatal(err)
		}
		if delivery == nil {
			return
		}
	}
	f.t.Fatal("outbox did not drain")
}

func (f *answerStreamFixture) reply() []*ExternalConversationDelivery {
	f.t.Helper()
	deliveries, err := f.replies.ProcessScope(f.t.Context(), f.endpoint.Scope, 10)
	if err != nil {
		f.t.Fatal(err)
	}
	return deliveries
}

func (f *answerStreamFixture) finish(status AgentRunStatus, answer string) {
	f.t.Helper()
	ctx := f.t.Context()
	if answer != "" {
		conversations := NewConversationService(f.store)
		current, _ := conversations.GetConversation(ctx, f.endpoint.Scope, f.conversation.ID)
		if _, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: f.endpoint.Scope, ConversationID: f.conversation.ID, ExpectedRevision: current.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: f.endpoint.DeploymentID},
			Intent: MessageIntentAnswer, Content: answer, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: f.inbound.ID, References: []ConversationReference{{Kind: ConversationReferenceRun, ID: f.run.ID}},
			IdempotencyKey: "streamed-answer",
		}); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, _, err := NewRunActivityService(f.store, f.store).TransitionRun(ctx, f.endpoint.Scope, f.run.ID, RunTransitionRequest{
		ExpectedRevision: f.run.Revision, Status: status, Summary: "Finished", Actor: ActivityActor{Type: "worker", ID: "test"},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *answerStreamFixture) operations() []capability.ConversationDeliveryOperation {
	operations := []capability.ConversationDeliveryOperation{}
	for _, request := range f.host.requests {
		operations = append(operations, request.Delivery.Operation)
	}
	return operations
}

const streamedOpening = "The deployment failed because the health check"

func TestExternalAnswerStreamEditsOneMessageAndFinishesIt(t *testing.T) {
	f := newAnswerStreamFixture(t, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
	f.preview("The deploy", false)
	if deliveries := f.stream(); len(deliveries) != 0 {
		t.Fatalf("a too-short preview was posted: %#v", deliveries)
	}
	f.preview(streamedOpening, false)
	opened := f.stream()
	if len(opened) != 1 || opened[0].Operation != capability.ConversationDeliveryMessageSend || opened[0].ChannelMessageID != f.inbound.ID {
		t.Fatalf("stream did not open one message: %#v", opened)
	}
	f.preview(streamedOpening+" timed out", false)
	if deliveries := f.stream(); len(deliveries) != 0 {
		t.Fatalf("an edit overtook the unsent message: %#v", deliveries)
	}
	f.deliverAll()
	if f.host.messages["chat-1"] != streamedOpening || f.host.requests[0].Message.Intent != MessageIntentAnswer || len(f.host.requests[0].Message.References) != 0 {
		t.Fatalf("preview body = %#v", f.host.requests[0].Message)
	}
	// Edits are throttled per conversation.
	f.clock = f.clock.Add(time.Second)
	if deliveries := f.stream(); len(deliveries) != 0 {
		t.Fatalf("edit was not throttled: %#v", deliveries)
	}
	f.clock = f.clock.Add(time.Second)
	edited := f.stream()
	if len(edited) != 1 || edited[0].Operation != capability.ConversationDeliveryMessageUpdate || edited[0].Parameters["providerMessageId"] != "chat-1" {
		t.Fatalf("stream did not edit its message: %#v", edited)
	}
	f.finish(AgentRunStatusCompleted, streamedOpening+" timed out after 30 seconds.")
	// The committed reply waits for the in-flight edit instead of racing it.
	if deliveries := f.reply(); len(deliveries) != 0 {
		t.Fatalf("reply overtook the stream: %#v", deliveries)
	}
	f.deliverAll()
	// A restarted reply worker finds the streamed message in the outbox.
	replies, err := NewExternalConversationReplyWorker(f.store, f.catalog)
	if err != nil {
		t.Fatal(err)
	}
	f.replies = replies
	f.reply()
	final, err := f.store.GetExternalConversationDelivery(t.Context(), f.endpoint.Scope, stableExternalConversationID(f.endpoint.Scope, f.endpoint.ID, "delivery", externalConversationReplyKey("progress-inbox")))
	if err != nil || final == nil || final.Operation != capability.ConversationDeliveryMessageUpdate || final.Parameters["providerMessageId"] != "chat-1" {
		t.Fatalf("final reply = %#v, %v", final, err)
	}
	f.preview(streamedOpening+" timed out after", false)
	f.deliverAll()
	if deliveries := f.reply(); len(deliveries) != 1 || deliveries[0].ID != final.ID {
		t.Fatalf("finished reply = %#v", deliveries)
	}
	if deliveries := f.stream(); len(deliveries) != 0 {
		t.Fatalf("a finished Run kept streaming: %#v", deliveries)
	}
	if len(f.host.messages) != 1 || f.host.messages["chat-1"] != streamedOpening+" timed out after 30 seconds." {
		t.Fatalf("chat = %#v", f.host.messages)
	}
	mapping, err := f.store.GetExternalMessageMapping(t.Context(), f.endpoint.Scope, f.endpoint.ID, ExternalMessageOutbound, "chat-1")
	if err != nil || mapping == nil || mapping.ChannelMessageID != final.ChannelMessageID {
		t.Fatalf("streamed message maps to %#v, %v", mapping, err)
	}
}

func TestExternalAnswerStreamSupersedesLateEdit(t *testing.T) {
	f := newAnswerStreamFixture(t, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
	f.preview(streamedOpening, false)
	f.stream()
	f.deliverAll()
	f.clock = f.clock.Add(2 * time.Second)
	f.preview(streamedOpening+" timed out", false)
	late := f.stream()
	if len(late) != 1 {
		t.Fatalf("edit = %#v", late)
	}
	// Another replica queued the committed reply before this edit was sent.
	if _, err := f.replies.transport.Enqueue(t.Context(), EnqueueExternalConversationDeliveryRequest{
		Scope: f.endpoint.Scope, EndpointID: f.endpoint.ID, Operation: capability.ConversationDeliveryMessageUpdate,
		ConversationID: f.conversation.ID, ChannelMessageID: f.inbound.ID, Parameters: map[string]interface{}{"providerMessageId": "chat-1"},
		IdempotencyKey: externalConversationReplyKey("progress-inbox"),
	}); err != nil {
		t.Fatal(err)
	}
	f.deliverAll()
	stored, _ := f.store.GetExternalConversationDelivery(t.Context(), f.endpoint.Scope, late[0].ID)
	if stored.Status != ExternalConversationDeliveryFailed || stored.ErrorCode != "answer_superseded" {
		t.Fatalf("late edit = %#v", stored)
	}
}

func TestExternalAnswerStreamFailedTurn(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		f := newAnswerStreamFixture(t, capability.ConversationDeliveryMessageDelete, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
		f.preview(streamedOpening, false)
		f.stream()
		f.deliverAll()
		f.preview("", true)
		f.finish(AgentRunStatusFailed, "")
		f.reply()
		f.deliverAll()
		if got := f.operations(); len(got) != 3 || got[1] != capability.ConversationDeliveryMessageDelete || got[2] != capability.ConversationDeliveryMessageSend {
			t.Fatalf("operations = %v", got)
		}
		if len(f.host.messages) != 1 || f.host.messages["chat-2"] == "" || f.host.messages["chat-2"] == streamedOpening {
			t.Fatalf("chat = %#v", f.host.messages)
		}
	})
	t.Run("edit", func(t *testing.T) {
		f := newAnswerStreamFixture(t, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
		f.preview(streamedOpening, false)
		f.stream()
		f.deliverAll()
		f.preview("", true)
		f.finish(AgentRunStatusFailed, "")
		f.reply()
		f.deliverAll()
		if got := f.operations(); len(got) != 2 || got[1] != capability.ConversationDeliveryMessageUpdate {
			t.Fatalf("operations = %v", got)
		}
		if len(f.host.messages) != 1 || f.host.messages["chat-1"] == streamedOpening {
			t.Fatalf("partial preview kept: %#v", f.host.messages)
		}
	})
}

func TestExternalAnswerStreamPostsReplyWhenEditFails(t *testing.T) {
	f := newAnswerStreamFixture(t, capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate)
	f.preview(streamedOpening, false)
	f.stream()
	f.deliverAll()
	f.finish(AgentRunStatusCompleted, streamedOpening+" timed out.")
	// The chat message was deleted, so the edit cannot finish it.
	f.host.rejectAll = capability.ConversationDeliveryMessageUpdate
	f.reply()
	f.deliverAll()
	f.reply()
	f.deliverAll()
	if got := f.operations(); len(got) != 3 || got[2] != capability.ConversationDeliveryMessageSend || f.host.messages["chat-2"] != streamedOpening+" timed out." {
		t.Fatalf("operations = %v, chat = %#v", got, f.host.messages)
	}
	if deliveries := f.reply(); len(deliveries) != 0 {
		t.Fatalf("resolved reply was projected again: %#v", deliveries)
	}
}

func TestExternalAnswerStreamNeedsMessageUpdate(t *testing.T) {
	f := newAnswerStreamFixture(t, capability.ConversationDeliveryMessageSend)
	f.preview(streamedOpening, false)
	if deliveries := f.stream(); len(deliveries) != 0 {
		t.Fatalf("an adapter without message.update streamed: %#v", deliveries)
	}
	f.finish(AgentRunStatusCompleted, streamedOpening+" timed out.")
	if deliveries := f.reply(); len(deliveries) != 1 || deliveries[0].Operation != capability.ConversationDeliveryMessageSend {
		t.Fatalf("reply = %#v", deliveries)
	}
}

func TestExternalAnswerStreamTextStaysBelowChatLimits(t *testing.T) {
	if got := externalAnswerStreamText("  short  "); got != "" {
		t.Fatalf("short preview = %q", got)
	}
	long := externalAnswerStreamText(strings.Repeat("word ", 2000))
	if units := len([]rune(long)); units > externalAnswerStreamDisplayUnits || !strings.HasSuffix(long, "word…") {
		t.Fatalf("long preview has %d characters, ends %q", units, long[len(long)-12:])
	}
}
