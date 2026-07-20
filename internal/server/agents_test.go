package server

import (
	"context"
	"encoding/json"
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
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"id":"agent-definitions","version":"5"`) || !strings.Contains(capabilities.Body.String(), `"update"`) {
		t.Fatalf("capabilities = %d %s", capabilities.Code, capabilities.Body.String())
	}

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

	entry := kernelapi.AgentDeploymentCatalogEntry{}
	if err := json.Unmarshal(detail.Body.Bytes(), &entry); err != nil || entry.Deployment == nil {
		t.Fatalf("decode detail = %#v, %v", entry, err)
	}
	proposed := *entry.Deployment
	proposed.RolloutStatus = kernelagent.RolloutPaused
	proposed.Credentials = map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "vault://tenant-one/provider"}}
	payload, _ := json.Marshal(kernelapi.UpdateAgentDeploymentRequest{
		Deployment: &proposed, ExpectedRevision: proposed.Revision, ActorType: "system", ActorID: "reconciler", Reason: "place provider",
	})
	updated := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/agent-deployments/operator-live", string(payload), "")
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"rolloutStatus":"paused"`) ||
		!strings.Contains(updated.Body.String(), `"changeKind":"configuration-updated"`) || !strings.Contains(updated.Body.String(), `"vault://tenant-one/provider"`) {
		t.Fatalf("update = %d %s", updated.Code, updated.Body.String())
	}
	stale := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/agent-deployments/operator-live", string(payload), "")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale update = %d %s", stale.Code, stale.Body.String())
	}
	mismatch := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/agent-deployments/other", string(payload), "")
	if mismatch.Code != http.StatusBadRequest || !strings.Contains(mismatch.Body.String(), "path and payload ids") {
		t.Fatalf("path mismatch = %d %s", mismatch.Code, mismatch.Body.String())
	}
	unknown := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/agent-deployments/operator-live", `{"deployment":null,"expectedRevision":2,"actorType":"system","actorId":"reconciler","reason":"update","credential":"raw"}`, "")
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "unknown field") || strings.Contains(unknown.Body.String(), "raw") {
		t.Fatalf("unknown field = %d %s", unknown.Code, unknown.Body.String())
	}
}
