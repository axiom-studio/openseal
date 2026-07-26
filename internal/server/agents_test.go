package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	server := NewServer(store, zap.NewNop().Sugar())
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"id":"agent-definitions","version":"7"`) ||
		!strings.Contains(capabilities.Body.String(), `"update"`) || !strings.Contains(capabilities.Body.String(), `"rollback"`) ||
		!strings.Contains(capabilities.Body.String(), `"propose-amendment"`) {
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
	proposed.Credentials = map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "credential://tenant-one/provider"}}
	payload, _ := json.Marshal(kernelapi.UpdateAgentDeploymentRequest{
		Deployment: &proposed, ExpectedRevision: proposed.Revision, ActorType: "system", ActorID: "reconciler", Reason: "place provider",
	})
	updated := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/agent-deployments/operator-live", string(payload), "")
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"rolloutStatus":"paused"`) ||
		!strings.Contains(updated.Body.String(), `"changeKind":"configuration-updated"`) || !strings.Contains(updated.Body.String(), `"credential://tenant-one/provider"`) {
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

func TestAgentDefinitionLifecycleIsCASGuardedScopedAndAudited(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "agent-lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	registry := kernelagent.NewRegistryWithStore(store)
	base := &kernelagent.AgentDefinition{
		ID: "operator", Version: "1.0.0", DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: "Inspect before acting.",
		Authority:  kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 1},
		Amendments: kernelagent.AmendmentPolicy{AllowedFields: []string{"systemPrompt"}, RequiresApproval: true, ApproverPrincipals: []string{"user:admin"}},
	}
	registered, err := registry.RegisterDefinition(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	second := *registered
	second.Version = "2.0.0"
	second.SystemPrompt = "Inspect, test, and then act."
	second.Digest = ""
	second.CreatedAt = time.Time{}
	if _, err := registry.RegisterDefinition(ctx, &second); err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	deployed, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "operator-live", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "production", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "initial activation")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, zap.NewNop().Sugar())

	activatePayload, _ := json.Marshal(kernelapi.ActivateAgentDefinitionRequest{
		Scope: scope, Version: "2.0.0", ExpectedRevision: deployed.Revision, ActorType: "user", ActorID: "admin", Reason: "validated rollout",
	})
	activated := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-deployments/operator-live/activations", string(activatePayload), "")
	if activated.Code != http.StatusOK || !strings.Contains(activated.Body.String(), `"activeVersion":"2.0.0"`) ||
		!strings.Contains(activated.Body.String(), `"reason":"validated rollout"`) {
		t.Fatalf("activate = %d %s", activated.Code, activated.Body.String())
	}
	stale := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-deployments/operator-live/activations", string(activatePayload), "")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale activation = %d %s", stale.Code, stale.Body.String())
	}
	history := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/operator-live/activations?scopeKind=tenant&scopeId=one", "", "")
	if history.Code != http.StatusOK || strings.Count(history.Body.String(), `"deploymentRevision"`) != 2 {
		t.Fatalf("activation history = %d %s", history.Code, history.Body.String())
	}
	foreignHistory := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/operator-live/activations?scopeKind=tenant&scopeId=two", "", "")
	if foreignHistory.Code != http.StatusNotFound {
		t.Fatalf("foreign activation history = %d %s", foreignHistory.Code, foreignHistory.Body.String())
	}

	rollbackPayload, _ := json.Marshal(kernelapi.RollbackAgentDefinitionRequest{
		Scope: scope, ExpectedRevision: deployed.Revision + 1, ActorType: "user", ActorID: "admin", Reason: "quality regression",
	})
	rolledBack := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-deployments/operator-live/rollbacks", string(rollbackPayload), "")
	if rolledBack.Code != http.StatusOK || !strings.Contains(rolledBack.Body.String(), `"activeVersion":"1.0.0"`) ||
		!strings.Contains(rolledBack.Body.String(), `"reason":"quality regression"`) {
		t.Fatalf("rollback = %d %s", rolledBack.Code, rolledBack.Body.String())
	}

	candidate := *registered
	candidate.Version = "1.1.0"
	candidate.SystemPrompt = "Inspect evidence and ask before acting."
	candidate.Digest = ""
	candidate.CreatedAt = time.Time{}
	proposalPayload, _ := json.Marshal(kernelagent.ProposeAmendmentRequest{
		Scope: scope, DeploymentID: "operator-live", Candidate: &candidate, ProposerType: "user", ProposerID: "author", Rationale: "Require explicit approval.",
	})
	proposed := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-deployments/operator-live/amendments", string(proposalPayload), "")
	if proposed.Code != http.StatusCreated || !strings.Contains(proposed.Body.String(), `"status":"awaiting_approval"`) {
		t.Fatalf("proposal = %d %s", proposed.Code, proposed.Body.String())
	}
	var amendment kernelagent.DefinitionAmendment
	if err := json.Unmarshal(proposed.Body.Bytes(), &amendment); err != nil {
		t.Fatal(err)
	}
	listed := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/operator-live/amendments?scopeKind=tenant&scopeId=one", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), amendment.ID) {
		t.Fatalf("amendments = %d %s", listed.Code, listed.Body.String())
	}
	wrongDeployment := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/agent-deployments/other/amendments/"+amendment.ID+"?scopeKind=tenant&scopeId=one", "", "")
	if wrongDeployment.Code != http.StatusNotFound {
		t.Fatalf("wrong deployment amendment = %d %s", wrongDeployment.Code, wrongDeployment.Body.String())
	}

	decisionPayload, _ := json.Marshal(kernelagent.ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Approved: true,
		ActorType: "user", ActorID: "admin", Reason: "reviewed",
	})
	approved := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-deployments/operator-live/amendments/"+amendment.ID+"/decisions", string(decisionPayload), "")
	if approved.Code != http.StatusOK || !strings.Contains(approved.Body.String(), `"status":"approved"`) {
		t.Fatalf("approve = %d %s", approved.Code, approved.Body.String())
	}
	var approvedAmendment kernelagent.DefinitionAmendment
	if err := json.Unmarshal(approved.Body.Bytes(), &approvedAmendment); err != nil {
		t.Fatal(err)
	}
	amendmentActivationPayload, _ := json.Marshal(kernelapi.ActivateAgentDefinitionAmendmentRequest{
		Scope: scope, ExpectedRevision: approvedAmendment.Revision, ActorType: "user", ActorID: "admin", Reason: "approved amendment",
	})
	amended := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/agent-deployments/operator-live/amendments/"+amendment.ID+"/activations", string(amendmentActivationPayload), "")
	if amended.Code != http.StatusOK || !strings.Contains(amended.Body.String(), `"status":"activated"`) ||
		!strings.Contains(amended.Body.String(), `"activeVersion":"1.1.0"`) || !strings.Contains(amended.Body.String(), `"deploymentRevision":4`) {
		t.Fatalf("amendment activation = %d %s", amended.Code, amended.Body.String())
	}
}
