package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

func TestSkillActionDiscoveryReturnsExactSecretSafeSemanticBindings(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	definition := &skill.Definition{
		ID: "community", Version: "1", Name: "Community", Transport: skill.TransportReference{Kind: "remote-node"},
		Actions: map[string]skill.Action{"reply": {
			Name: "reply", Description: "Reply to a community thread", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal,
			Idempotency: skill.IdempotencyRequired, SemanticArguments: map[string]string{"target": "thread", "body": "message"},
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
				"thread": map[string]interface{}{"type": "string"}, "message": map[string]interface{}{"type": "string"},
			}, "required": []interface{}{"thread", "message"}},
		}},
	}
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "community-account", Scope: scope, DeploymentID: "researcher", SkillID: definition.ID, SkillVersion: definition.Version,
		AllowedActions: []string{"reply"}, MaximumRisk: skill.RiskLevelExternal,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "oauth", ID: "opaque-secret-reference"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	api := NewServer(nil, nil, store, zap.NewNop().Sugar())
	response := performAgentRunRequest(t, api.Handler(), http.MethodGet,
		"/api/v1/agent-deployments/researcher/skill-actions?scopeKind=tenant&scopeId=one&semanticRole=target&semanticRole=body&sideEffect=external", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"bindingId":"community-account"`) ||
		!strings.Contains(response.Body.String(), `"bindingRevision":1`) || strings.Contains(response.Body.String(), "opaque-secret-reference") || strings.Contains(response.Body.String(), "credentials") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	crossScope := performAgentRunRequest(t, api.Handler(), http.MethodGet,
		"/api/v1/agent-deployments/researcher/skill-actions?scopeKind=tenant&scopeId=two&semanticRole=target&semanticRole=body", "", "")
	if crossScope.Code != http.StatusOK || !strings.Contains(crossScope.Body.String(), `"actions":[]`) {
		t.Fatalf("cross scope=%d %s", crossScope.Code, crossScope.Body.String())
	}
}
