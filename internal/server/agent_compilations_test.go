package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestAgentCompilationAPIIsCapabilityAdvertisedAndScopeIsolated(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "compilations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	registry := kernelagent.NewRegistryWithStore(store)
	definition, err := registry.RegisterDefinition(ctx, &kernelagent.AgentDefinition{ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate", SystemPrompt: "Operate safely.", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: "operator", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "local", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RecordCompilation(ctx, &kernelagent.DefinitionCompilation{ID: "source-1", Scope: scope, DeploymentID: "operator", DefinitionID: definition.ID, CandidateVersion: "1", Source: kernelagent.CompilationSource{Kind: "prompt", ID: "source", Version: "1", Digest: "sha256:source"}, TargetDigest: definition.Digest, Status: kernelagent.CompilationClean}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, store, zap.NewNop().Sugar())
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), kernelapi.AgentDefinitionsCapabilityID) || !strings.Contains(capabilities.Body.String(), kernelapi.OperationListCompilations) {
		t.Fatalf("capabilities = %d %s", capabilities.Code, capabilities.Body.String())
	}
	listed := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/operator/compilations?scopeKind=local&scopeId=default", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"source-1"`) || !strings.Contains(listed.Body.String(), `"status":"clean"`) {
		t.Fatalf("compilations = %d %s", listed.Code, listed.Body.String())
	}
	foreign := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/operator/compilations?scopeKind=local&scopeId=other", "", "")
	if foreign.Code != http.StatusNotFound || strings.Contains(foreign.Body.String(), "source-1") {
		t.Fatalf("foreign compilations = %d %s", foreign.Code, foreign.Body.String())
	}
}
