package server

import (
	"net/http"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestChannelTimelineQueryRejectsAmbiguousValues(t *testing.T) {
	store := runtime.NewMemoryStore()
	service := runtime.NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), runtime.CreateConversationRequest{
		Scope: runtime.Scope{Kind: "tenant", ID: "one"}, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "team"},
		Title: "Timeline", IdempotencyKey: "timeline",
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(store, zap.NewNop().Sugar())
	base := "/api/v1/conversations/" + conversation.ID + "/messages?scopeKind=tenant&scopeId=one"
	for _, query := range []string{"channelTimeline=", "channelTimeline=1", "channelTimeline=True", "channelTimeline=false&channelTimeline=true", "channelTimeline=true&threadRootId=root"} {
		t.Run(query, func(t *testing.T) {
			response := performAgentRunRequest(t, api.Handler(), http.MethodGet, base+"&"+query, "", "")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
	for _, query := range []string{"", "&channelTimeline=false", "&channelTimeline=true", "&channelTimeline=false&threadRootId=root"} {
		response := performAgentRunRequest(t, api.Handler(), http.MethodGet, base+query, "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("query %q status = %d, body = %s", query, response.Code, response.Body.String())
		}
	}
}
