package server

import (
	"context"
	"encoding/json"
	"net/http"
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
