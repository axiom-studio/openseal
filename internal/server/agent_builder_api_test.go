package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func newAgentBuilderTestServer(t *testing.T, withBuilder bool) (*Server, *runtime.SQLiteStore) {
	t.Helper()
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "builder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if withBuilder {
		ctx := context.Background()
		registry := kernelagent.NewRegistryWithStore(store)
		definition, err := registry.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
			ID: "builder", Version: "1", DisplayName: "Finn", Purpose: "Build agents", SystemPrompt: "Build agents.",
			Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
			ID: runtime.AgentBuilderDeploymentID, Scope: capability.ScopeReference{Kind: "local", ID: "default"},
			DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive,
			Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "local-operator", "test"); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(store, zap.NewNop().Sugar())
	server.SetDesktopConversationScope(runtime.Scope{Kind: "local", ID: "default"})
	return server, store
}

func TestAgentBuilderCapabilityOnlyWithBuilderDeployment(t *testing.T) {
	without, _ := newAgentBuilderTestServer(t, false)
	response := performAgentRunRequest(t, without.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"agent-builder"`) {
		t.Fatalf("capabilities without builder = %d %s", response.Code, response.Body.String())
	}
	start := performAgentRunRequest(t, without.Handler(), http.MethodPost, "/api/v1/agent-builder/conversations",
		`{"scope":{"kind":"local","id":"default"},"prompt":"Hi","parameters":{"kind":"agent"},"idempotencyKey":"k"}`, "")
	if start.Code != http.StatusNotFound {
		t.Fatalf("start without builder = %d %s", start.Code, start.Body.String())
	}

	with, _ := newAgentBuilderTestServer(t, true)
	response = performAgentRunRequest(t, with.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range document.Capabilities {
		if entry.ID == kernelapi.AgentBuilderCapabilityID {
			found = entry.Version == "1" && entry.Context != nil && entry.Context.BuilderAgentID == runtime.AgentBuilderDeploymentID &&
				strings.Join(entry.Operations, ",") == kernelapi.OperationCreate+","+kernelapi.OperationList
		}
	}
	if !found {
		t.Fatalf("capabilities with builder = %s", response.Body.String())
	}
}

func TestAgentBuilderRoutesStartAndListThroughClient(t *testing.T) {
	server, _ := newAgentBuilderTestServer(t, true)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	kernel := client.NewKernelHTTPClient(httpServer.URL, nil)
	ctx := context.Background()
	scope := runtime.Scope{Kind: "local", ID: "default"}
	request := kernelapi.StartAgentBuilderConversationRequest{
		Scope: scope, Prompt: "An agent that triages my inbox",
		Parameters: runtime.AgentBuilderParameters{Kind: runtime.AgentBuilderKindAgent, Name: "Triage", Model: "openai/gpt-5"}, IdempotencyKey: "start-1",
	}
	started, err := kernel.StartAgentBuilderConversation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if started.Replayed || started.Conversation == nil || started.Conversation.Origin == nil ||
		started.Conversation.Origin.Kind != runtime.ConversationReferenceAgentBuilder || started.Conversation.Owner.ID != runtime.AgentBuilderDeploymentID ||
		started.Message == nil || started.Message.Content != request.Prompt || started.Message.Sender != localBuilderRequester ||
		started.Message.BuilderParameters == nil || started.Message.BuilderParameters.Name != "Triage" || started.Message.BuilderParameters.Model != "openai/gpt-5" {
		t.Fatalf("started = %#v", started)
	}
	replayed, err := kernel.StartAgentBuilderConversation(ctx, request)
	if err != nil || !replayed.Replayed || replayed.Message.ID != started.Message.ID {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	drafts, err := kernel.ListAgentBuilderDrafts(ctx, scope, 10, 0)
	if err != nil || len(drafts.Drafts) != 1 || drafts.Drafts[0].ConversationID != started.Conversation.ID ||
		drafts.Drafts[0].Status != runtime.AgentBuilderDraftIdle || drafts.Drafts[0].Title != "Triage" {
		t.Fatalf("drafts = %#v, %v", drafts, err)
	}
	if page, err := kernel.ListAgentBuilderDrafts(ctx, scope, 10, 1); err != nil || len(page.Drafts) != 0 {
		t.Fatalf("offset page = %#v, %v", page, err)
	}

	for name, body := range map[string]string{
		"sensitive": `{"scope":{"kind":"local","id":"default"},"prompt":"Password: hunter2-secret","parameters":{"kind":"agent"},"idempotencyKey":"s"}`,
		"draft":     `{"scope":{"kind":"local","id":"default"},"prompt":"Hi","parameters":{"kind":"agent","draft":{"purpose":"x"}},"idempotencyKey":"d"}`,
		"unknown":   `{"scope":{"kind":"local","id":"default"},"prompt":"Hi","parameters":{"kind":"agent"},"idempotencyKey":"u","requester":"x"}`,
	} {
		response := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-builder/conversations", body, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", name, response.Code, response.Body.String())
		}
	}
	foreign := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-builder/conversations",
		`{"scope":{"kind":"tenant","id":"other"},"prompt":"Hi","parameters":{"kind":"agent"},"idempotencyKey":"f"}`, "")
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("foreign scope = %d %s", foreign.Code, foreign.Body.String())
	}
	for _, query := range []string{"scopeKind=local&scopeId=default&limit=101", "scopeKind=local&scopeId=default&offset=-1", "scopeKind=tenant&scopeId=other"} {
		response := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-builder/drafts?"+query, "", "")
		if response.Code != http.StatusBadRequest && response.Code != http.StatusForbidden {
			t.Fatalf("drafts %s = %d %s", query, response.Code, response.Body.String())
		}
	}
}
