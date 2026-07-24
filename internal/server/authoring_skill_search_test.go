package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestWorkforceSkillSearchIsCapabilityGatedAndNormalized(t *testing.T) {
	api := NewServer(runtime.NewMemoryStore(), zap.NewNop().Sugar())
	path := "/api/v1/authoring/workforce/skills?scopeKind=tenant&scopeId=one&query=summary"
	if response := performAgentRunRequest(t, api.Handler(), http.MethodGet, path, "", ""); response.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured search = %d %s", response.Code, response.Body.String())
	}
	api.SetWorkforceSkillSearchProvider(authoring.SkillSearchProviderFunc(func(_ context.Context, request authoring.SkillSearchRequest) (*authoring.SkillSearchPage, error) {
		if request.Query != "summary" || request.Limit != 20 || request.Scope.ID != "one" {
			t.Fatalf("request = %#v", request)
		}
		return &authoring.SkillSearchPage{Items: []authoring.SkillSearchCandidate{{
			SkillCapability: authoring.SkillCapability{
				ID: "summary", Version: "1.0.0", SourceIdentity: "installed:summary@1.0.0",
				Name: "Summary", Readiness: authoring.SkillReadinessReady, MaximumRisk: capability.RiskLevelRead,
			},
			Origin: authoring.SkillSearchOriginEnabled, Verification: authoring.SkillSearchVerificationVerified,
		}}}, nil
	}))
	response := performAgentRunRequest(t, api.Handler(), http.MethodGet, path, "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"sourceIdentity":"installed:summary@1.0.0"`) {
		t.Fatalf("search = %d %s", response.Code, response.Body.String())
	}
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if !strings.Contains(capabilities.Body.String(), `"search"`) {
		t.Fatalf("capabilities = %s", capabilities.Body.String())
	}
}
