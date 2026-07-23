package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

func TestAgentTurnAPIProjectsScopeSafeOperationalTimeline(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryStore(20)
	scope := runtime.Scope{Kind: "local", ID: "workspace"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		AssignedAgentID: "operator", Goal: "Inspect the cluster", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = runtime.NewRunActivityService(store, store).TransitionRun(
		t.Context(), scope, run.ID,
		runtime.RunTransitionRequest{ExpectedRevision: run.Revision, Status: runtime.AgentRunStatusRunning},
	)
	if err != nil {
		t.Fatal(err)
	}
	turnService := runtime.NewAgentTurnService(store, store)
	turn, err := turnService.BeginTurn(t.Context(), runtime.BeginAgentTurnRequest{
		Scope: scope, RunID: run.ID, DefinitionID: "sre", DefinitionVersion: "4",
		ModelProvider: "hosted", Model: "bounded", WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turnService.FinishTurn(t.Context(), scope, turn.ID, runtime.FinishAgentTurnRequest{
		ExpectedRevision: turn.Revision, Status: runtime.AgentTurnStatusCompleted, WorkerID: "worker-1",
		ModelProvider: "hosted", Model: "bounded", OutputSummary: "Inspected warning events",
		Decisions: []runtime.TurnDecision{{
			Summary: "Inspect before changing production", Rationale: "private-reasoning-must-not-leak",
		}},
		RequestedActions: []runtime.TurnAction{{
			Type: "skill", Capability: "kubernetes.events", BindingID: "cluster-reader",
			Summary: "Read warning events", InputRef: "/continuationCheckpoint/secret-input",
			PreparedRuntime: &skill.PreparedRuntime{RuntimeID: "private-runtime-id"},
		}},
		ContinuationCheckpoint: map[string]interface{}{"secret-input": "private-action-input"},
		RunOutput:              map[string]interface{}{"private-output": true},
	})
	if err != nil {
		t.Fatal(err)
	}

	api := NewServer(nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	kernel := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())

	document, err := kernel.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.AgentTurnsCapabilityID, kernelapi.AgentTurnsCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationGet) || !capability.Supports(kernelapi.OperationList) {
		t.Fatalf("agent turn capability = %#v", capability)
	}
	listed, err := kernel.ListAgentTurns(t.Context(), runtime.AgentTurnFilter{
		Scope: scope, RunID: run.ID, Statuses: []runtime.AgentTurnStatus{runtime.AgentTurnStatusCompleted}, Limit: 10,
	})
	if err != nil || len(listed) != 1 || listed[0].ID != turn.ID || listed[0].OutputSummary != "Inspected warning events" {
		t.Fatalf("listed turns = %#v, %v", listed, err)
	}
	if len(listed[0].Decisions) != 1 || len(listed[0].RequestedActions) != 1 {
		t.Fatalf("turn audit projection = %#v", listed[0])
	}
	if after, err := kernel.ListAgentTurns(t.Context(), runtime.AgentTurnFilter{
		Scope: scope, RunID: run.ID, AfterSequence: turn.Sequence, Limit: 10,
	}); err != nil || len(after) != 0 {
		t.Fatalf("cursor page = %#v, %v", after, err)
	}
	restored, err := kernel.GetAgentTurn(t.Context(), scope, turn.ID)
	if err != nil || restored.ID != turn.ID {
		t.Fatalf("restored turn = %#v, %v", restored, err)
	}
	_, err = kernel.GetAgentTurn(t.Context(), runtime.Scope{Kind: "local", ID: "other"}, turn.ID)
	var apiError *client.APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-scope turn error = %#v", err)
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/agent-turns/"+turn.ID+"?scopeKind=local&scopeId=workspace", nil)
	api.Handler().ServeHTTP(response, request)
	var wire map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	encoded := response.Body.String()
	for _, forbidden := range []string{
		"continuationCheckpoint", "preparedRuntime", "inputRef", "runOutput",
		"private-reasoning-must-not-leak", "private-action-input", "private-runtime-id",
	} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("public turn projection leaked %q: %s", forbidden, encoded)
		}
	}
}
