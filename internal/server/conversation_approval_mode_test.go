package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"go.uber.org/zap"
)

func TestConversationApprovalModeAPI(t *testing.T) {
	store := kerneltest.NewStore(t)
	seedSpace(t, store, models.Scope{Kind: "tenant", ID: "one"}, localOperatorID)
	server := NewServer(store, zap.NewNop().Sugar())
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations",
		`{"scope":{"kind":"tenant","id":"one"},"owner":{"type":"agent","id":"assistant"},"title":"Chat"}`, "chat")
	var conversation models.Conversation
	if err := json.NewDecoder(created.Body).Decode(&conversation); err != nil {
		t.Fatal(err)
	}
	if conversation.ApprovalMode != models.ConversationApprovalAuto {
		t.Fatalf("created approval mode = %q", conversation.ApprovalMode)
	}
	path := "/api/v1/conversations/" + conversation.ID + "/approval-mode"
	body := func(mode string, revision int64) string {
		value, _ := json.Marshal(kernelapi.SetConversationApprovalModeRequest{
			Scope: conversation.Scope, ExpectedRevision: revision, Mode: models.ConversationApprovalMode(mode),
			Actor: models.ActivityActor{Type: "user", ID: "operator"},
		})
		return string(value)
	}
	if response := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, body("never", conversation.Revision), ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, body("manual", conversation.Revision+1), ""); response.Code != http.StatusConflict {
		t.Fatalf("stale revision status = %d, body = %s", response.Code, response.Body.String())
	}
	response := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, body("manual", conversation.Revision), "")
	if response.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", response.Code, response.Body.String())
	}
	var result runtimeconversation.SetConversationApprovalModeResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Conversation.ApprovalMode != models.ConversationApprovalManual || result.Event == nil ||
		result.Event.EventType != runtimeconversation.ApprovalModeChangedEvent {
		t.Fatalf("update result = %#v", result)
	}
	read := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/conversations/"+conversation.ID+"?scopeKind=tenant&scopeId=one", "", "")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"approvalMode":"manual"`) {
		t.Fatalf("read status = %d, body = %s", read.Code, read.Body.String())
	}
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if !strings.Contains(capabilities.Body.String(), kernelapi.OperationApprovalMode) {
		t.Fatalf("capabilities do not advertise approval mode: %s", capabilities.Body.String())
	}
}
