package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestChannelListReadPositionsAreScopedOptionalAndReadOnly(t *testing.T) {
	store := runtime.NewMemoryStore()
	service := runtime.NewConversationService(store)
	local := runtime.Scope{Kind: "local", ID: "default"}
	reader := runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "local-operator"}
	channel, _, err := service.CreateConversation(context.Background(), runtime.CreateConversationRequest{Scope: local, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "research"}, Title: "Evidence", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PostChannelMessage(context.Background(), runtime.PostChannelMessageRequest{Scope: local, ConversationID: channel.ID, ExpectedRevision: 1, Sender: reader, Intent: runtime.MessageIntentUpdate, Content: "New evidence", IdempotencyKey: "message", Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, zap.NewNop().Sugar())
	base := "/api/v1/conversations?scopeKind=local&scopeId=default&ownerType=team&ownerId=research"
	plain := performAgentRunRequest(t, server.Handler(), http.MethodGet, base, "", "")
	if plain.Code != http.StatusOK || strings.Contains(plain.Body.String(), `"readPosition"`) {
		t.Fatal(plain.Body.String())
	}
	projection := base + "&participantType=user&participantId=local-operator"
	unread := performAgentRunRequest(t, server.Handler(), http.MethodGet, projection, "", "")
	if unread.Code != http.StatusOK || !strings.Contains(unread.Body.String(), `"readSequence":0,"revision":0`) {
		t.Fatal(unread.Body.String())
	}
	if cursor, err := service.GetCursor(context.Background(), local, channel.ID, reader); err != nil || cursor != nil {
		t.Fatalf("listing wrote a receipt: %#v %v", cursor, err)
	}
	if _, _, err := service.AdvanceCursor(context.Background(), runtime.AdvanceConversationCursorRequest{Scope: local, ConversationID: channel.ID, Participant: reader, DeliveredSequence: 1, ReadSequence: 1}); err != nil {
		t.Fatal(err)
	}
	read := performAgentRunRequest(t, server.Handler(), http.MethodGet, projection, "", "")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"readSequence":1,"revision":1`) {
		t.Fatal(read.Body.String())
	}
	other := performAgentRunRequest(t, server.Handler(), http.MethodGet, strings.Replace(projection, "local-operator", "other", 1), "", "")
	if other.Code != http.StatusOK || !strings.Contains(other.Body.String(), `"readSequence":0,"revision":0`) {
		t.Fatal(other.Body.String())
	}
	foreign := performAgentRunRequest(t, server.Handler(), http.MethodGet, strings.Replace(projection, "scopeId=default", "scopeId=foreign", 1), "", "")
	if foreign.Code != http.StatusOK || strings.TrimSpace(foreign.Body.String()) != "[]" {
		t.Fatal(foreign.Body.String())
	}
	for _, query := range []string{"&participantType=user", "&participantId=local-operator", "&participantType=invalid&participantId=local-operator"} {
		invalid := performAgentRunRequest(t, server.Handler(), http.MethodGet, base+query, "", "")
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("partial/invalid reader: %d", invalid.Code)
		}
	}
	for _, title := range []string{"Second", "Third"} {
		if _, _, err := service.CreateConversation(context.Background(), runtime.CreateConversationRequest{Scope: local, Owner: channel.Owner, Title: title, IdempotencyKey: title}); err != nil {
			t.Fatal(err)
		}
	}
	page := performAgentRunRequest(t, server.Handler(), http.MethodGet, projection+"&limit=1&offset=1", "", "")
	var items []json.RawMessage
	if err := json.Unmarshal(page.Body.Bytes(), &items); err != nil || len(items) != 1 {
		t.Fatalf("pagination %s %v", page.Body.String(), err)
	}
}

type failingReadSummaryStore struct{ *runtime.MemoryStore }

func (s failingReadSummaryStore) GetConversationCursor(context.Context, runtime.Scope, string, runtime.ConversationParticipant) (*runtime.ConversationCursor, error) {
	return nil, errors.New("read storage unavailable")
}
func TestChannelListDoesNotInventUnreadCountsOnCursorFailure(t *testing.T) {
	store := failingReadSummaryStore{runtime.NewMemoryStore()}
	service := runtime.NewConversationService(store)
	if _, _, err := service.CreateConversation(context.Background(), runtime.CreateConversationRequest{Scope: runtime.Scope{Kind: "local", ID: "default"}, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "research"}, Title: "Evidence", IdempotencyKey: "channel"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, zap.NewNop().Sugar())
	response := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/conversations?scopeKind=local&scopeId=default&participantType=user&participantId=local-operator", "", "")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "readPosition") {
		t.Fatalf("invented read position: %d %s", response.Code, response.Body.String())
	}
}
