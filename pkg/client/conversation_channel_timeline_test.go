package client

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/runtimetest"
	"go.uber.org/zap"
)

func TestKernelHTTPClientChannelTimelineRoundTrip(t *testing.T) {
	store := runtimetest.NewKernelStore(t)
	service := runtimeconversation.NewConversationService(store)
	scope := models.Scope{Kind: "tenant", ID: "timeline"}
	// The kernel creates no space: a conversation made with no person goes
	// to the empty owner's default space, so the test seeds it.
	if _, err := portfolio.NewSpaceService(store).CreateSpace(t.Context(), portfolio.CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: models.DefaultSpaceLook,
	}); err != nil {
		t.Fatal(err)
	}
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, Title: "Timeline", IdempotencyKey: "timeline",
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(content, reply string, broadcast bool) *models.ChannelMessage {
		t.Helper()
		result, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "sender"},
			Intent: models.MessageIntentUpdate, Content: content, IdempotencyKey: content,
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: reply, BroadcastToChannel: broadcast,
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
	filter := models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, ChannelTimeline: true, Limit: 1, Descending: true}
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
