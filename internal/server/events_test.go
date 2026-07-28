package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestEventAPIProjectsCanonicalRunsAndRejectsProtocolDrift(t *testing.T) {
	store := runtime.NewMemoryStore()
	server := NewServer(store, zap.NewNop().Sugar())
	objectiveBody := `{
		"scope":{"kind":"tenant","id":"operations"},
		"owner":{"type":"agent","id":"sre"},
		"title":"Production reliability",
		"goal":"Investigate Kubernetes warnings safely",
		"status":"active"
	}`
	createdObjective := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/objectives", objectiveBody, "sre-objective")
	if createdObjective.Code != http.StatusCreated {
		t.Fatalf("objective = %d %s", createdObjective.Code, createdObjective.Body.String())
	}
	var objective runtime.Objective
	if err := json.NewDecoder(createdObjective.Body).Decode(&objective); err != nil {
		t.Fatal(err)
	}
	activation, err := runtime.NewRunbookActivationService(store).Create(context.Background(), runtime.CreateRunbookActivationRequest{
		ID: "backoff", Scope: objective.Scope, Owner: objective.Owner, ObjectiveID: objective.ID, AssignedAgentID: "sre",
		DefinitionID: "kubernetes-investigation", DefinitionVersion: "1", TriggerID: "backoff",
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent, EventType: "kubernetes.warning", Entrypoint: "investigate"},
		Input:   map[string]interface{}{"mode": "evidence-first"}, Status: runtime.RunbookActivationActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventBody := `{
		"id":"event-uid-1","scope":{"kind":"tenant","id":"operations"},
		"type":"kubernetes.warning","source":"cluster:production","subject":"Deployment/checkout",
		"severity":"Warning","occurredAt":"2026-07-14T12:00:00Z",
		"attributes":{"reason":"BackOff"},"payload":{"message":"container restarted"}
	}`
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/events", eventBody, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("event = %d %s", created.Code, created.Body.String())
	}
	var result runtime.EventRouteResult
	if err := json.NewDecoder(created.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Routes) != 1 || !result.Routes[0].Created || result.Routes[0].Run.ObjectiveID != objective.ID || result.Routes[0].Run.Source != runtime.RunSourceEvent {
		t.Fatalf("event routes = %#v", result)
	}
	if pin, ok := result.Routes[0].Run.Plan["runbook"].(map[string]interface{}); !ok || pin["id"] != activation.DefinitionID || pin["version"] != activation.DefinitionVersion || pin["trigger"] != activation.TriggerID {
		t.Fatalf("event Run pin=%#v", result.Routes[0].Run.Plan)
	}
	replay := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/events", eventBody, "")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), result.Routes[0].Run.ID) {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	drift := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/events", strings.TrimSuffix(eventBody, "}")+`,"executeDirectly":true}`, "")
	if drift.Code != http.StatusBadRequest {
		t.Fatalf("protocol drift = %d %s", drift.Code, drift.Body.String())
	}
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.EventRoutingCapabilityID, kernelapi.EventRoutingCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationRoute) {
		t.Fatalf("event capability = %#v", document)
	}
}
