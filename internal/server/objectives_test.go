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

func TestObjectiveAPIExposesIdempotentPortfolioLifecycleAndRuns(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	server := NewServer(nil, nil, store, zap.NewNop().Sugar())
	server.SetAgentRunCreationDispatcher(runtime.NewRunCommandService(store).CreateAgentRun)
	body := `{"scope":{"kind":"tenant","id":"one"},"owner":{"type":"team","id":"gtm"},"title":"Launch","goal":"Create demand","status":"active","budget":{"maxAttempts":5,"maxTurns":10,"maxTotalTokens":1000,"maxDurationMs":120000}}`
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/objectives", body, "launch-objective")
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var objective runtime.Objective
	if err := json.NewDecoder(created.Body).Decode(&objective); err != nil {
		t.Fatal(err)
	}
	if objective.Budget == nil || objective.Budget.MaxAttempts != 5 || objective.Budget.MaxTurns != 10 || objective.Budget.MaxDurationMS != 120000 || objective.Status != runtime.ObjectiveStatusActive {
		t.Fatalf("objective = %#v", objective)
	}
	replayed := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/objectives", body, "launch-objective")
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), objective.ID) {
		t.Fatalf("replay = %d %s", replayed.Code, replayed.Body.String())
	}
	conflictBody := strings.Replace(body, "Create demand", "Different goal", 1)
	conflict := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/objectives", conflictBody, "launch-objective")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	runBody := `{"scope":{"kind":"tenant","id":"one"},"objectiveId":"` + objective.ID + `","owner":{"type":"team","id":"gtm"},"assignedAgentId":"marketer","goal":"Draft launch","source":"objective","budget":{"maxAttempts":2,"maxTurns":4,"maxTotalTokens":400,"maxDurationMs":60000}}`
	runResponse := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-runs", runBody, "launch-run")
	if runResponse.Code != http.StatusCreated {
		t.Fatalf("run = %d %s", runResponse.Code, runResponse.Body.String())
	}
	detailResponse := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/objectives/"+objective.ID+"?scopeKind=tenant&scopeId=one", "", "")
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", detailResponse.Code, detailResponse.Body.String())
	}
	var detail kernelapi.ObjectiveDetail
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.Objective == nil || len(detail.Runs) != 1 || len(detail.Objective.BudgetAllocations) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
	paused := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/objectives/"+objective.ID+"?scopeKind=tenant&scopeId=one", `{"expectedRevision":2,"status":"paused"}`, "")
	if paused.Code != http.StatusOK || !strings.Contains(paused.Body.String(), `"status":"paused"`) {
		t.Fatalf("pause = %d %s", paused.Code, paused.Body.String())
	}
	list := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/objectives?scopeKind=tenant&scopeId=one&ownerType=team&ownerId=gtm&status=paused", "", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), objective.ID) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	crossScope := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/objectives/"+objective.ID+"?scopeKind=tenant&scopeId=two", "", "")
	if crossScope.Code != http.StatusNotFound {
		t.Fatalf("cross scope = %d %s", crossScope.Code, crossScope.Body.String())
	}
}

func TestObjectiveAPIRejectsInvalidLifecycleTransition(t *testing.T) {
	store := runtime.NewMemoryStore(20)
	server := NewServer(nil, nil, store, zap.NewNop().Sugar())
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/objectives", `{"scope":{"kind":"local","id":"default"},"owner":{"type":"agent","id":"one"},"title":"Done","goal":"Finish","status":"satisfied"}`, "done")
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var objective runtime.Objective
	_ = json.NewDecoder(created.Body).Decode(&objective)
	updated := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/objectives/"+objective.ID+"?scopeKind=local&scopeId=default", `{"expectedRevision":1,"status":"active"}`, "")
	if updated.Code != http.StatusConflict {
		t.Fatalf("invalid transition = %d %s", updated.Code, updated.Body.String())
	}
}
