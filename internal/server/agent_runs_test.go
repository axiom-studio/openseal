package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestAgentRunAPIUsesCanonicalCommands(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	server := NewServer(nil, nil, store, zap.NewNop().Sugar())
	createBody := `{
		"scope":{"kind":"tenant","id":"one"},
		"kind":"conversation",
		"owner":{"type":"team","id":"team-1"},
		"concurrencyKey":"channel:engineering",
		"goal":"Operate the service",
		"source":"manual",
		"budgetPolicy":{"maxTurns":12,"maxTotalTokens":50000,"maxCostMicros":2500000},
		"actor":{"type":"user","id":"7"}
	}`
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-runs", createBody, "request-one")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var createResult runtime.AgentRunCommandResult
	if err := json.NewDecoder(created.Body).Decode(&createResult); err != nil {
		t.Fatal(err)
	}
	if createResult.Run == nil || createResult.Run.Kind != runtime.RunKindConversation || createResult.Run.ConcurrencyKey != "channel:engineering" ||
		createResult.Run.BudgetPolicy == nil || createResult.Run.BudgetPolicy.MaxTurns != 12 || createResult.Run.BudgetState != runtime.BudgetStateActive ||
		createResult.Event == nil || createResult.Event.EventType != "run.created" {
		t.Fatalf("create result = %#v", createResult)
	}

	replayed := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-runs", createBody, "request-one")
	if replayed.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", replayed.Code, replayed.Body.String())
	}
	var replayResult runtime.AgentRunCommandResult
	if err := json.NewDecoder(replayed.Body).Decode(&replayResult); err != nil {
		t.Fatal(err)
	}
	if replayResult.Run.ID != createResult.Run.ID || replayResult.Event != nil {
		t.Fatalf("replay result = %#v", replayResult)
	}

	changedBody := strings.Replace(createBody, "Operate the service", "Different work", 1)
	conflict := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-runs", changedBody, "request-one")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, body = %s", conflict.Code, conflict.Body.String())
	}

	list := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-runs?scopeKind=tenant&scopeId=one&kind=conversation&ownerType=team&ownerId=team-1", "", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", list.Code, list.Body.String())
	}
	var runs []*runtime.AgentRun
	if err := json.NewDecoder(list.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != createResult.Run.ID {
		t.Fatalf("listed runs = %#v", runs)
	}

	wrongScope := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-runs/"+createResult.Run.ID+"?scopeKind=tenant&scopeId=two", "", "")
	if wrongScope.Code != http.StatusNotFound {
		t.Fatalf("cross-scope get status = %d, body = %s", wrongScope.Code, wrongScope.Body.String())
	}

	commandBody := `{"expectedRevision":1,"kind":"pause","actor":{"type":"user","id":"7"}}`
	paused := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-runs/"+createResult.Run.ID+"/commands?scopeKind=tenant&scopeId=one", commandBody, "")
	if paused.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body = %s", paused.Code, paused.Body.String())
	}
	var pauseResult runtime.AgentRunCommandResult
	if err := json.NewDecoder(paused.Body).Decode(&pauseResult); err != nil {
		t.Fatal(err)
	}
	if pauseResult.Run.Status != runtime.AgentRunStatusPaused || pauseResult.Event.EventType != "run.paused" {
		t.Fatalf("pause result = %#v", pauseResult)
	}

	invalid := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-runs", strings.TrimSuffix(createBody, "}")+`,"unknown":true}`, "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, body = %s", invalid.Code, invalid.Body.String())
	}
}

func TestCapabilitiesAdvertiseAgentRunOperations(t *testing.T) {
	server := NewServer(nil, nil, runtime.NewMemoryStore(10), zap.NewNop().Sugar())
	recorder := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"agent-runs"`) || !strings.Contains(recorder.Body.String(), `"intervene"`) {
		t.Fatalf("capabilities status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func performAgentRunRequest(t *testing.T, handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}
