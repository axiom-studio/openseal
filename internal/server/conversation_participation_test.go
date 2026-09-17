package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestChannelParticipationRequiresHostAuthorityAndCurrentRevision(t *testing.T) {
	store := runtime.NewMemoryStore()
	service := runtime.NewConversationService(store)
	scope := runtime.Scope{Kind: "local", ID: "default"}
	channel, _, err := service.CreateConversation(t.Context(), runtime.CreateConversationRequest{Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "team"}, Title: "Research", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, zap.NewNop().Sugar())
	server.SetDesktopConversationScope(scope)
	patch := func(revision int64, enabled bool, scopeID string) int {
		body := fmt.Sprintf(`{"scope":{"kind":"local","id":%q},"expectedRevision":%d,"participationEnabled":%t}`, scopeID, revision, enabled)
		return performAgentRunRequest(t, server.Handler(), http.MethodPatch, "/api/v1/conversations/"+channel.ID, body, "").Code
	}
	if got := patch(1, true, "default"); got != http.StatusForbidden {
		t.Fatalf("unconfigured host: %d", got)
	}
	forged := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations/"+channel.ID+"/participation-rounds", `{"scope":{"kind":"local","id":"default"},"expectedRevision":1,"proposals":[]}`, "forged")
	if forged.Code != http.StatusForbidden {
		t.Fatalf("desktop accepted model-authored proposal injection: %d", forged.Code)
	}
	allowed := true
	checks := 0
	server.SetChannelParticipation(func(_ context.Context, current *runtime.Conversation) error {
		checks++
		if current.ID != channel.ID || current.Scope != scope {
			t.Fatal("wrong canonical channel")
		}
		if !allowed {
			return errors.New("provider unavailable")
		}
		return nil
	}, nil)
	if got := patch(1, true, "foreign"); got != http.StatusForbidden || checks != 0 {
		t.Fatalf("foreign scope admitted: %d", got)
	}
	if got := patch(1, true, "default"); got != http.StatusOK {
		t.Fatalf("enable: %d", got)
	}
	if got := patch(1, false, "default"); got != http.StatusConflict {
		t.Fatalf("stale disable: %d", got)
	}
	allowed = false
	if got := patch(2, false, "default"); got != http.StatusOK || checks != 1 {
		t.Fatalf("disable required provider access: %d checks=%d", got, checks)
	}
	if got := patch(3, true, "default"); got != http.StatusUnprocessableEntity {
		t.Fatalf("missing provider enabled: %d", got)
	}
	saved, err := service.GetConversation(t.Context(), scope, channel.ID)
	if err != nil || saved.Participation.Enabled || saved.Revision != 3 {
		t.Fatalf("rejected enable changed state: %#v %v", saved, err)
	}
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if !strings.Contains(capabilities.Body.String(), "configure-participation") || strings.Contains(capabilities.Body.String(), "coordinate-automatically") {
		t.Fatal(capabilities.Body.String())
	}
}

func TestChannelPostDispatchRunsAfterDesktopAuth(t *testing.T) {
	store := runtime.NewMemoryStore()
	service := runtime.NewConversationService(store)
	scope := runtime.Scope{Kind: "local", ID: "default"}
	channel, _, err := service.CreateConversation(t.Context(), runtime.CreateConversationRequest{Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "team"}, Title: "Research", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, zap.NewNop().Sugar())
	server.SetDesktopConversationScope(scope)
	calls := 0
	server.SetChannelParticipation(func(context.Context, *runtime.Conversation) error { return nil }, func(ctx context.Context, req runtime.PostChannelMessageRequest) (*runtime.ChannelMessageCommitResult, error) {
		calls++
		return service.PostChannelMessage(ctx, req)
	})
	for _, sender := range []string{"forged", "local-operator"} {
		payload := map[string]interface{}{"scope": scope, "expectedRevision": 1, "sender": runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: sender}, "intent": "question", "content": "Review this", "audience": map[string]string{"kind": "channel"}, "idempotencyKey": sender}
		body, _ := json.Marshal(payload)
		response := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations/"+channel.ID+"/messages", string(body), sender)
		if sender == "forged" && (response.Code != http.StatusForbidden || calls != 0) || sender == "local-operator" && (response.Code != http.StatusCreated || calls != 1) {
			t.Fatalf("dispatch authority: %d calls=%d", response.Code, calls)
		}
	}
}
