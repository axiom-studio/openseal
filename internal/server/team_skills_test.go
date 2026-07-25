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
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"go.uber.org/zap"
)

func TestTeamSkillBindingAPIIsDurableScopedAuditedAndCASGuarded(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "team-skills.db")
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistryWithStore(store)
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "analyst", Version: "1", DisplayName: "Analyst", Purpose: "Analyze", SystemPrompt: "Analyze.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "analyst-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "create roster")
	if err != nil {
		t.Fatal(err)
	}
	teams := kernelteam.NewRegistryWithStore(store, agents)
	teamDefinition, err := teams.RegisterDefinition(ctx, &kernelteam.Definition{
		ID: "research", Version: "1", DisplayName: "Research", Purpose: "Research",
		Roles: []kernelteam.RoleSlot{{
			ID: "analyst", DisplayName: "Analyst", Purpose: "Analyze", MinimumMembers: 1,
			SkillGrants: []kernelteam.RoleSkillGrant{
				{SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"search"}, MaximumRisk: capability.RiskLevelRead},
				{SkillID: "forum", SkillVersion: "2", AllowedActions: []string{"search"}, MaximumRisk: capability.RiskLevelRead},
			},
		}},
		Coordination: kernelteam.CoordinationPolicy{}, Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "research-one", Scope: scope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version, Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "analyst", RoleID: "analyst", AgentDeploymentID: agentDeployment.ID}},
	}, "user", "operator", "activate Team"); err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "forum", Version: "1", Name: "Forum", Transport: capability.TransportReference{Kind: "local"},
		Actions: map[string]capability.Action{"search": {Name: "search", Description: "Search", Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "forum", Version: "2", Name: "Forum", Transport: capability.TransportReference{Kind: "local"},
		Actions: map[string]capability.Action{"search": {Name: "search", Description: "Search current sources", Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(store, zap.NewNop().Sugar())
	agentRequest := skill.UpsertBindingRequest{
		Binding: &skill.Binding{SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"search"}, MaximumRisk: capability.RiskLevelRead},
		Actor:   skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Agent needs scoped research access",
	}
	agentPayload, _ := json.Marshal(agentRequest)
	agentCreated := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/agent-deployments/analyst-one/skill-bindings/forum?scopeKind=tenant&scopeId=one", string(agentPayload), "")
	if agentCreated.Code != http.StatusCreated || !strings.Contains(agentCreated.Body.String(), `"deploymentId":"analyst-one"`) {
		t.Fatalf("create Agent binding = %d %s", agentCreated.Code, agentCreated.Body.String())
	}
	request := skill.UpsertBindingRequest{
		Binding: &skill.Binding{SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"search"}, MaximumRisk: capability.RiskLevelRead, Credentials: map[string]capability.CredentialReference{"FORUM_TOKEN": {Kind: "api-token", ID: "credential-17"}}},
		Actor:   skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Team uses shared research account",
	}
	payload, _ := json.Marshal(request)
	created := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/team-deployments/research-one/skill-bindings/forum?scopeKind=tenant&scopeId=one", string(payload), "")
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"deploymentId":"research-one"`) || !strings.Contains(created.Body.String(), `"action":"created"`) {
		t.Fatalf("create Team binding = %d %s", created.Code, created.Body.String())
	}
	planPayload, _ := json.Marshal(runtime.PlanSkillReferenceUpgradeRequest{ToVersion: "2"})
	planned := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/team-deployments/research-one/skill-bindings/forum/upgrade-plan?scopeKind=tenant&scopeId=one", string(planPayload), "")
	if planned.Code != http.StatusOK || !strings.Contains(planned.Body.String(), `"version":"2"`) || !strings.Contains(planned.Body.String(), `"authorizedRoleIds":["analyst"]`) {
		t.Fatalf("plan Team binding upgrade = %d %s", planned.Code, planned.Body.String())
	}
	var plan runtime.SkillReferenceUpgradePlan
	if err := json.Unmarshal(planned.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	applyPayload, _ := json.Marshal(runtime.ApplySkillReferenceUpgradeRequest{
		Plan: &plan, Actor: runtime.ActivityActor{Type: "user", ID: "operator"}, Reason: "reviewed exact forum upgrade",
	})
	applied := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/team-deployments/research-one/skill-bindings/forum/upgrade?scopeKind=tenant&scopeId=one", string(applyPayload), "")
	if applied.Code != http.StatusOK || !strings.Contains(applied.Body.String(), `"bindingRevision":2`) || !strings.Contains(applied.Body.String(), `"reason":"reviewed exact forum upgrade"`) {
		t.Fatalf("apply Team binding upgrade = %d %s", applied.Code, applied.Body.String())
	}
	replayed := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/team-deployments/research-one/skill-bindings/forum/upgrade?scopeKind=tenant&scopeId=one", string(applyPayload), "")
	if replayed.Code != http.StatusBadRequest && replayed.Code != http.StatusConflict {
		t.Fatalf("replay Team binding upgrade = %d %s", replayed.Code, replayed.Body.String())
	}
	stale := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/team-deployments/research-one/skill-bindings/forum?scopeKind=tenant&scopeId=one", string(payload), "")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale Team binding = %d %s", stale.Code, stale.Body.String())
	}
	foreign := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/team-deployments/research-one/skill-bindings?scopeKind=tenant&scopeId=other", "", "")
	if foreign.Code != http.StatusNotFound || strings.Contains(foreign.Body.String(), "credential-17") {
		t.Fatalf("cross-tenant Team binding = %d %s", foreign.Code, foreign.Body.String())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restartedStore, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedStore.Close()
	restarted := NewServer(restartedStore, zap.NewNop().Sugar())
	listed := performAgentRunRequest(t, restarted.Handler(), http.MethodGet, "/api/v1/team-deployments/research-one/skill-bindings?scopeKind=tenant&scopeId=one", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"revision":2`) || !strings.Contains(listed.Body.String(), `"skillVersion":"2"`) || !strings.Contains(listed.Body.String(), `"id":"credential-17"`) {
		t.Fatalf("restarted Team binding = %d %s", listed.Code, listed.Body.String())
	}
	disable, _ := json.Marshal(skill.DisableBindingRequest{ExpectedRevision: 2, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "rotate account"})
	disabled := performAgentRunRequest(t, restarted.Handler(), http.MethodPost, "/api/v1/team-deployments/research-one/skill-bindings/forum/disable?scopeKind=tenant&scopeId=one", string(disable), "")
	if disabled.Code != http.StatusOK || !strings.Contains(disabled.Body.String(), `"disabled":true`) || !strings.Contains(disabled.Body.String(), `"revision":3`) || !strings.Contains(disabled.Body.String(), `"action":"disabled"`) {
		t.Fatalf("disable Team binding = %d %s", disabled.Code, disabled.Body.String())
	}
}
