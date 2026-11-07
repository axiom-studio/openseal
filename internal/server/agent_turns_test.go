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
	"go.uber.org/zap"
)

func TestAgentTurnAPIProjectsScopeSafeOperationalTimeline(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryStore()
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
		Scope: scope, RunID: run.ID, SessionID: "ses-1", Producer: runtime.StepProducerModel,
		DefinitionID: "sre", DefinitionVersion: "4", ModelProvider: "hosted", Model: "bounded", WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turnService.RecordStepModel(t.Context(), scope, turn.ID, runtime.StepModelRecord{
		ExpectedRevision: turn.Revision, WorkerID: "worker-1", ModelProvider: "hosted", Model: "bounded",
		AssistantEntryID: "private-assistant-entry", OutputSummary: "Inspected warning events",
		ToolCallCount: 2, StopReason: string(runtime.ModelStopToolCalls),
		SkillSelections: []runtime.HostedSkillSelection{{}},
		Usage:           runtime.TurnUsage{InputTokens: 1840, OutputTokens: 64, ProviderDurationMS: 910},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err = turnService.FinishTurn(t.Context(), scope, turn.ID, runtime.FinishAgentTurnRequest{
		ExpectedRevision: turn.Revision, Status: runtime.AgentTurnStatusCompleted, WorkerID: "worker-1",
		Usage: runtime.TurnUsage{InputTokens: 1840, OutputTokens: 64, ProviderDurationMS: 910},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := turnService.BeginTurn(t.Context(), runtime.BeginAgentTurnRequest{
		Scope: scope, RunID: run.ID, SessionID: "ses-1", Producer: runtime.StepProducerModel,
		ModelProvider: "hosted", Model: "bounded", WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = turnService.FinishTurn(t.Context(), scope, failed.ID, runtime.FinishAgentTurnRequest{
		ExpectedRevision: failed.Revision, Status: runtime.AgentTurnStatusFailed, WorkerID: "worker-1",
		FailureCode: "host_unavailable", Error: "model host unreachable",
	})
	if err != nil {
		t.Fatal(err)
	}

	api := NewServer(store, zap.NewNop().Sugar())
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
	step := listed[0]
	if step.SessionID != "ses-1" || step.Producer != runtime.StepProducerModel || step.ToolCallCount != 2 ||
		step.StopReason != "tool_calls" || step.Final || step.FailureCode != "" || step.Usage.InputTokens != 1840 {
		t.Fatalf("step projection = %#v", step)
	}
	failedListed, err := kernel.ListAgentTurns(t.Context(), runtime.AgentTurnFilter{
		Scope: scope, RunID: run.ID, Statuses: []runtime.AgentTurnStatus{runtime.AgentTurnStatusFailed}, Limit: 10,
	})
	if err != nil || len(failedListed) != 1 || failedListed[0].ID != failed.ID ||
		failedListed[0].FailureCode != "host_unavailable" || failedListed[0].Error != "model host unreachable" {
		t.Fatalf("failed steps = %#v, %v", failedListed, err)
	}
	if after, err := kernel.ListAgentTurns(t.Context(), runtime.AgentTurnFilter{
		Scope: scope, RunID: run.ID, AfterSequence: turn.Sequence, Limit: 10,
	}); err != nil || len(after) != 1 || after[0].ID != failed.ID {
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
	if _, ok := wire["toolCallCount"]; !ok {
		t.Fatalf("public turn projection lacks toolCallCount: %s", encoded)
	}
	for _, forbidden := range []string{
		"decisions", "requestedActions", "skillSelections", "planRevision", "nextRunStatus", "wakeCondition",
		"runError", "assistantEntryId", "leaseOwner", "continuationCheckpoint", "preparedRuntime", "inputRef", "runOutput",
		"private-reasoning-must-not-leak", "private-action-input", "private-runtime-id", "private-assistant-entry",
	} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("public turn projection leaked %q: %s", forbidden, encoded)
		}
	}
}
