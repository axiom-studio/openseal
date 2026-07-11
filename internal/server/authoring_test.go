package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

type authoringFixtureGenerator struct{}

func (authoringFixtureGenerator) Generate(context.Context, authoring.GenerateRequest) ([]byte, error) {
	return []byte(`{"candidate":{"agents":[],"assignments":[]},"questions":["Which responsibilities should this Team own?"]}`), nil
}

func TestWorkforceChangeSetAPIIsDurableScopedAndIdempotent(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(nil, nil, store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(authoringFixtureGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if !strings.Contains(capabilities.Body.String(), `"operations":["compile","propose","get"]`) {
		t.Fatalf("change set capability = %s", capabilities.Body.String())
	}
	body := `{"scope":{"kind":"tenant","id":"one"},"prompt":"Create a research Team","catalog":{},"placement":{"teamDeploymentId":"research-live","agentDeploymentIds":{}},"actor":{"type":"user","id":"7"}}`
	created := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets", body, "intent-one")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var changeSet authoring.ChangeSet
	if err := json.NewDecoder(created.Body).Decode(&changeSet); err != nil || changeSet.ID == "" || changeSet.Status != authoring.ChangeSetBlocked {
		t.Fatalf("change set = %#v, err = %v", changeSet, err)
	}
	replay := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets", body, "intent-one")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), changeSet.ID) {
		t.Fatalf("replay status = %d, body = %s", replay.Code, replay.Body.String())
	}
	loaded := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/authoring/workforce/change-sets/"+changeSet.ID+"?scopeKind=tenant&scopeId=one", "", "")
	if loaded.Code != http.StatusOK || !strings.Contains(loaded.Body.String(), changeSet.CandidateDigest) {
		t.Fatalf("load status = %d, body = %s", loaded.Code, loaded.Body.String())
	}
	foreign := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/authoring/workforce/change-sets/"+changeSet.ID+"?scopeKind=tenant&scopeId=two", "", "")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status = %d, body = %s", foreign.Code, foreign.Body.String())
	}
}

func TestWorkforceAuthoringAPIIsTruthfulAndNonActivating(t *testing.T) {
	api := NewServer(nil, nil, runtime.NewMemoryStore(10), zap.NewNop().Sugar())
	unconfigured := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/compile", `{"mode":"create","prompt":"Create a Team","catalog":{}}`, "")
	if unconfigured.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured status = %d, body = %s", unconfigured.Code, unconfigured.Body.String())
	}

	compiler, err := authoring.NewCompiler(authoringFixtureGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	api.SetWorkforceAuthoringCompiler(compiler)
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.WorkforceAuthoringCapabilityID, kernelapi.WorkforceAuthoringCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationCompile) || capability.Supports(kernelapi.OperationActivate) {
		t.Fatalf("authoring capability = %#v", capability)
	}

	compiled := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/compile", `{"mode":"create","prompt":"Create a Team","catalog":{}}`, "")
	if compiled.Code != http.StatusOK {
		t.Fatalf("compile status = %d, body = %s", compiled.Code, compiled.Body.String())
	}
	var result authoring.CompileResult
	if err := json.NewDecoder(compiled.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Valid || len(result.Questions) != 1 || len(result.Validation) == 0 {
		t.Fatalf("compile result = %#v", result)
	}
}
