package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestProjectAPIExposesIdempotentMultiObjectiveLifecycle(t *testing.T) {
	store := runtime.NewMemoryStore()
	server := NewServer(store, zap.NewNop().Sugar())
	objectiveBody := `{"scope":{"kind":"tenant","id":"one"},"owner":{"type":"team","id":"research"},"title":"Evidence","goal":"Collect evidence","status":"active"}`
	objectiveResponse := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/objectives", objectiveBody, "evidence-objective")
	if objectiveResponse.Code != http.StatusCreated {
		t.Fatalf("objective = %d %s", objectiveResponse.Code, objectiveResponse.Body.String())
	}
	var objective runtime.Objective
	if err := json.NewDecoder(objectiveResponse.Body).Decode(&objective); err != nil {
		t.Fatal(err)
	}
	body := `{"scope":{"kind":"tenant","id":"one"},"owner":{"type":"team","id":"research"},"title":"Market research","purpose":"Deliver a cited report","status":"active","objectiveRefs":["` + objective.ID + `"]}`
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects", body, "research-project")
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var project runtime.Project
	if err := json.NewDecoder(created.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	if project.Revision != 1 || project.Status != runtime.ProjectStatusActive || len(project.ObjectiveRefs) != 1 {
		t.Fatalf("project = %#v", project)
	}
	replayed := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects", body, "research-project")
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), project.ID) {
		t.Fatalf("replay = %d %s", replayed.Code, replayed.Body.String())
	}
	conflict := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects", strings.Replace(body, "cited report", "different report", 1), "research-project")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	paused := performAgentRunRequest(t, server.Handler(), http.MethodPatch, "/api/v1/projects/"+project.ID+"?scopeKind=tenant&scopeId=one", `{"expectedRevision":1,"status":"paused"}`, "")
	if paused.Code != http.StatusOK || !strings.Contains(paused.Body.String(), `"status":"paused"`) {
		t.Fatalf("pause = %d %s", paused.Code, paused.Body.String())
	}
	list := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects?scopeKind=tenant&scopeId=one&ownerType=team&ownerId=research&objectiveId="+objective.ID+"&status=paused", "", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), project.ID) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	crossScope := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/"+project.ID+"?scopeKind=tenant&scopeId=two", "", "")
	if crossScope.Code != http.StatusNotFound {
		t.Fatalf("cross scope = %d %s", crossScope.Code, crossScope.Body.String())
	}
	noOp := performAgentRunRequest(t, server.Handler(), http.MethodPatch, "/api/v1/projects/"+project.ID+"?scopeKind=tenant&scopeId=one", `{"expectedRevision":2}`, "")
	if noOp.Code != http.StatusBadRequest {
		t.Fatalf("no-op = %d %s", noOp.Code, noOp.Body.String())
	}
	legacyCreate := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects", strings.TrimSuffix(body, "}")+`,"runRefs":["run-a"]}`, "legacy-run-list")
	if legacyCreate.Code != http.StatusBadRequest || !strings.Contains(legacyCreate.Body.String(), "unknown field") {
		t.Fatalf("legacy Project Run list = %d %s", legacyCreate.Code, legacyCreate.Body.String())
	}
}
