package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestEventSourceSubscriptionAPIExposesDurableLifecycleHealthAndCheckpoint(t *testing.T) {
	store := runtime.NewMemoryStore()
	api := NewServer(store, zap.NewNop().Sugar())
	createBody := `{
		"id":"production-warnings","scope":{"kind":"tenant","id":"operations"},
		"owner":{"type":"agent","id":"sre"},"displayName":"Production warnings",
		"source":"kubernetes:cluster:production","connector":{"kind":"host","id":"kubernetes.watch","version":"1"},
		"status":"active","eventTypes":["kubernetes.warning"],"parameters":{"namespace":"production"}
	}`
	created := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/event-source-subscriptions", createBody, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var subscription runtime.EventSourceSubscription
	if err := json.NewDecoder(created.Body).Decode(&subscription); err != nil {
		t.Fatal(err)
	}
	if subscription.Revision != 1 || subscription.Status != runtime.EventSourceSubscriptionActive {
		t.Fatalf("subscription = %#v", subscription)
	}

	health := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/event-source-subscriptions/production-warnings/health-reports?scopeKind=tenant&scopeId=operations", `{"observedSubscriptionRevision":1,"state":"healthy"}`, "")
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"state":"healthy"`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}
	checkpoint := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/event-source-subscriptions/production-warnings/checkpoint-advancements?scopeKind=tenant&scopeId=operations", `{"expectedRevision":0,"observedSubscriptionRevision":1,"cursor":"rv-10","eventIds":["event-1"]}`, "")
	if checkpoint.Code != http.StatusOK || !strings.Contains(checkpoint.Body.String(), `"cursor":"rv-10"`) {
		t.Fatalf("checkpoint = %d %s", checkpoint.Code, checkpoint.Body.String())
	}
	detail := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/event-source-subscriptions/production-warnings?scopeKind=tenant&scopeId=operations", "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"health"`) || !strings.Contains(detail.Body.String(), `"checkpoint"`) {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
	list := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/event-source-subscriptions?scopeKind=tenant&scopeId=operations&status=active&connectorKind=host", "", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), subscription.ID) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	retired := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/event-source-subscriptions/production-warnings/retirements?scopeKind=tenant&scopeId=operations", `{"expectedRevision":1}`, "")
	if retired.Code != http.StatusOK || !strings.Contains(retired.Body.String(), `"status":"retired"`) {
		t.Fatalf("retire = %d %s", retired.Code, retired.Body.String())
	}
	crossScope := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/event-source-subscriptions/production-warnings?scopeKind=tenant&scopeId=other", "", "")
	if crossScope.Code != http.StatusNotFound {
		t.Fatalf("cross scope = %d %s", crossScope.Code, crossScope.Body.String())
	}
	drift := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/event-source-subscriptions", strings.TrimSuffix(createBody, "}")+`,"credential":"secret"}`, "")
	if drift.Code != http.StatusBadRequest {
		t.Fatalf("protocol drift = %d %s", drift.Code, drift.Body.String())
	}

	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.EventSourceSubscriptionsCapabilityID, kernelapi.EventSourceSubscriptionsCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationReportHealth) || !capability.Supports(kernelapi.OperationAdvanceCheckpoint) || !capability.Supports(kernelapi.OperationRetire) {
		t.Fatalf("capability = %#v", document)
	}
}
