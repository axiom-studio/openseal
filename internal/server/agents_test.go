package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestAgentDeploymentCatalogIsScopeIsolatedAndIncludesActiveDefinition(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	registry := kernelagent.NewRegistryWithStore(store)
	definition, err := registry.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: "Inspect before acting.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelProduction, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "operator-live", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "production", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "one", "test"); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil, store, zap.NewNop().Sugar())

	listed := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments?scopeKind=tenant&scopeId=one", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"operator-live"`) ||
		!strings.Contains(listed.Body.String(), `"displayName":"Operator"`) {
		t.Fatalf("catalog = %d %s", listed.Code, listed.Body.String())
	}
	foreign := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments?scopeKind=tenant&scopeId=two", "", "")
	if foreign.Code != http.StatusOK || strings.Contains(foreign.Body.String(), "operator-live") {
		t.Fatalf("foreign catalog = %d %s", foreign.Code, foreign.Body.String())
	}
	detail := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/operator-live?scopeKind=tenant&scopeId=one", "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"systemPrompt":"Inspect before acting."`) {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
	missing := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/operator-live?scopeKind=tenant&scopeId=two", "", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("foreign detail = %d %s", missing.Code, missing.Body.String())
	}
}
