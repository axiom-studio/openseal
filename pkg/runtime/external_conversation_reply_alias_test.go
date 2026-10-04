package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type replyAliasDispatcher struct{}

func (replyAliasDispatcher) DispatchExternalConversation(_ context.Context, req ExternalConversationDispatchRequest) (*ExternalConversationDispatchResult, error) {
	return &ExternalConversationDispatchResult{RunID: "reply-run-" + req.Message.ID}, nil
}

func receiveReplyAliasEvent(t *testing.T, store *MemoryStore, worker *ExternalConversationInboxWorker, endpoint *ExternalConversationEndpoint,
	chatID, messageID, threadID, parentID, text string,
) *ChannelMessage {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := &ExternalConversationInboxItem{
		ID:    "inbox:" + endpoint.Scope.ID + ":" + endpoint.ID + ":" + messageID,
		Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "event:" + messageID, Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: chatID, ExternalMessageID: messageID, ExternalThreadID: threadID, ReplyToExternalMessageID: parentID,
			ExternalParticipantID: "user:Kev", ParticipantDisplayName: "Kev", Text: text,
			OrderingKey: chatID + ":" + threadID, OccurredAt: now,
		},
		Status: ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := store.ReceiveExternalConversationEvent(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || applied == nil || applied.Status != ExternalConversationInboxApplied {
		t.Fatalf("apply reply alias = %#v, %v", applied, err)
	}
	message, err := store.GetChannelMessage(t.Context(), endpoint.Scope, applied.ConversationID, applied.ChannelMessageID)
	if err != nil || message == nil {
		t.Fatalf("applied canonical message = %#v, %v", message, err)
	}
	return message
}

func TestExternalConversationReplyAliasesKeepUserAndBotRepliesInOriginalThread(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	host := &recordingContextHost{result: &ExternalConversationContextResult{Status: ExternalConversationContextUnavailable}}
	worker, err := NewExternalConversationInboxWorker(store, replyAliasDispatcher{}, ExternalConversationInboxWorkerConfig{
		WorkerID: "reply-alias-worker", ContextHost: host, ContextCatalog: contextHistoryResolver{catalog},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := receiveReplyAliasEvent(t, store, worker, endpoint, "chat-A", "chat-A:100", "", "", "Original image request")
	reply := receiveReplyAliasEvent(t, store, worker, endpoint, "chat-A", "chat-A:101", "reply:100", "chat-A:100", "Please add a hat")
	if reply.ThreadRootID != root.ID || reply.ReplyToMessageID != root.ID {
		t.Fatalf("reply to user lost root: root=%#v, reply=%#v", root, reply)
	}
	service := NewConversationService(store)
	conversation, err := service.GetConversation(ctx, endpoint.Scope, root.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID},
		Intent: MessageIntentAnswer, Content: "The image now has a hat.", ReplyToMessageID: reply.ID,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "bot-answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.SaveExternalMessageMapping(ctx, &ExternalMessageMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: ExternalMessageOutbound,
		ExternalMessageID: "chat-A:102", ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	// The alias must already resolve to the canonical root before any provider
	// context read, so returned history cannot accidentally start a new thread.
	host.afterRead = func() {
		mapping, err := store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, "chat-A", "reply:102")
		if err != nil || mapping == nil || mapping.ThreadRootMessageID != root.ID {
			t.Fatalf("root was not established before hydration: %#v, %v", mapping, err)
		}
	}
	other := receiveReplyAliasEvent(t, store, worker, endpoint, "chat-A", "chat-A:104", "", "", "Unrelated conversation topic")
	continued := receiveReplyAliasEvent(t, store, worker, endpoint, "chat-A", "chat-A:103", "reply:102", "chat-A:102", "Make that hat blue")
	if continued.ThreadRootID != root.ID || continued.ReplyToMessageID != posted.Message.ID || continued.ConversationID != root.ConversationID {
		t.Fatalf("reply to bot forked thread: root=%#v, continued=%#v", root, continued)
	}
	for _, alias := range []string{"reply:100", "reply:102"} {
		mapping, err := store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, "chat-A", alias)
		if err != nil || mapping == nil || mapping.ThreadRootMessageID != root.ID {
			t.Fatalf("alias %q = %#v, %v", alias, mapping, err)
		}
	}
	history, err := service.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ThreadRootID: root.ID, Limit: 10,
	})
	if err != nil || len(history) != 4 || history[0].ID != root.ID || history[1].ID != reply.ID ||
		history[2].ID != posted.Message.ID || history[3].ID != continued.ID {
		t.Fatalf("thread history lost user/bot replies: %#v, %v", history, err)
	}
	for _, message := range history {
		if message.ID == other.ID {
			t.Fatal("unrelated channel message leaked into thread history")
		}
	}
}

func TestExternalConversationReplyAliasesNeverReuseForeignParents(t *testing.T) {
	for _, name := range []string{"another chat", "qualified ID collision", "another endpoint", "another scope", "unknown parent", "explicit topic"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store, _, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
			worker, err := NewExternalConversationInboxWorker(store, replyAliasDispatcher{}, ExternalConversationInboxWorkerConfig{WorkerID: "reply-alias-worker"})
			if err != nil {
				t.Fatal(err)
			}
			root := receiveReplyAliasEvent(t, store, worker, endpoint, "chat-A", "chat-A:100", "", "", "Private original topic")
			target := cloneExternalConversationEndpoint(endpoint)
			chatID, messageID, threadID, parentID := "chat-A", "chat-A:101", "reply:100", "chat-A:100"
			switch name {
			case "another chat":
				chatID, messageID = "chat-B", "chat-B:101"
			case "qualified ID collision":
				chatID, messageID, parentID = "chat-B", "chat-B:101", "chat-B:100"
			case "another endpoint":
				target.ID, target.IngressRoute = "other-endpoint", "other-route"
			case "another scope":
				target.Scope.ID, target.IngressRoute = "other-tenant", "other-route"
			case "unknown parent":
				threadID, parentID = "reply:999", "chat-A:999"
			case "explicit topic":
				threadID, parentID = "topic:100", ""
			}
			if target.ID != endpoint.ID || target.Scope != endpoint.Scope {
				if err := store.CreateExternalConversationEndpoint(ctx, target); err != nil {
					t.Fatal(err)
				}
			}
			message := receiveReplyAliasEvent(t, store, worker, target, chatID, messageID, threadID, parentID, "Independent request")
			if message.ThreadRootID != "" || message.ReplyToMessageID != "" {
				t.Fatalf("foreign or unavailable parent was reused: root=%#v, incoming=%#v", root, message)
			}
			mapping, err := store.GetExternalConversationMapping(ctx, target.Scope, target.ID, chatID, threadID)
			if err != nil || mapping == nil || mapping.ThreadRootMessageID != message.ID || mapping.ThreadRootMessageID == root.ID {
				t.Fatalf("independent alias root = %#v, %v", mapping, err)
			}
		})
	}
}

func TestNormalizedExternalConversationReplyParentIsOptionalAndValidated(t *testing.T) {
	event := NormalizedExternalConversationEvent{
		ID: "event", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "chat", ExternalMessageID: "chat:2", ExternalParticipantID: "person", Text: "hello",
		OrderingKey: "chat:reply:1", OccurredAt: time.Now().UTC(),
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("existing event contract rejected: %v", err)
	}
	event.ReplyToExternalMessageID = "chat:1"
	encoded, err := json.Marshal(event)
	if err != nil || !strings.Contains(string(encoded), `"replyToExternalMessageId":"chat:1"`) {
		t.Fatalf("portable reply parent = %s, %v", encoded, err)
	}
	var decoded NormalizedExternalConversationEvent
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.ReplyToExternalMessageID != event.ReplyToExternalMessageID || decoded.Validate() != nil {
		t.Fatalf("reply parent round trip = %#v, %v", decoded, err)
	}
	for _, invalid := range []string{" chat:1", "chat:1\n", strings.Repeat("x", 1025)} {
		event.ReplyToExternalMessageID = invalid
		if err := event.Validate(); err == nil {
			t.Fatalf("invalid reply parent %q was accepted", invalid)
		}
	}
}
