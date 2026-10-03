package client

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestKernelHTTPClientChannelTimelineRoundTrip(t *testing.T) {
	store := runtime.NewMemoryStore()
	service := runtime.NewConversationService(store)
	scope := runtime.Scope{Kind: "tenant", ID: "timeline"}
	conversation, _, err := service.CreateConversation(t.Context(), runtime.CreateConversationRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "team"}, Title: "Timeline", IdempotencyKey: "timeline",
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(content, reply string, broadcast bool) *runtime.ChannelMessage {
		t.Helper()
		result, err := service.PostChannelMessage(t.Context(), runtime.PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
			Sender: runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "sender"},
			Intent: runtime.MessageIntentUpdate, Content: content, IdempotencyKey: content,
			Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel}, ReplyToMessageID: reply, BroadcastToChannel: broadcast,
		})
		if err != nil {
			t.Fatal(err)
		}
		conversation = result.Conversation
		return result.Message
	}
	root := post("root", "", false)
	post("thread reply", root.ID, false)
	broadcast := post("broadcast", root.ID, true)
	api := server.NewServer(store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	t.Cleanup(httpServer.Close)
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	filter := runtime.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, ChannelTimeline: true, Limit: 1, Descending: true}
	page, err := client.ListChannelMessages(t.Context(), filter)
	if err != nil || len(page) != 1 || page[0].ID != broadcast.ID {
		t.Fatalf("timeline page = %#v, err = %v", page, err)
	}
	filter.BeforeSequence = broadcast.Sequence
	page, err = client.ListChannelMessages(t.Context(), filter)
	if err != nil || len(page) != 1 || page[0].ID != root.ID {
		t.Fatalf("older timeline page = %#v, err = %v", page, err)
	}
	filter.BeforeSequence, filter.AfterSequence, filter.Descending = 0, root.Sequence, false
	page, err = client.ListChannelMessages(t.Context(), filter)
	if err != nil || len(page) != 1 || page[0].ID != broadcast.ID {
		t.Fatalf("forward timeline page = %#v, err = %v", page, err)
	}
	filter.ThreadRootID = root.ID
	_, err = client.ListChannelMessages(t.Context(), filter)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("combined filters error = %#v", err)
	}
}
