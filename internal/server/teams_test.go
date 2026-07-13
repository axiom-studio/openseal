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
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"go.uber.org/zap"
)

func TestTeamDefinitionAPIUsesVersionedScopedControlPlane(t *testing.T) {
	ctx := context.Background()
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "teams.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := capability.ScopeReference{Kind: "workspace", ID: "local"}
	agents := kernelagent.NewRegistryWithStore(store)
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find evidence.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "Team roster")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, nil, store, zap.NewNop().Sugar())
	for _, version := range []string{"1", "2"} {
		definition := &kernelteam.Definition{
			ID: "research-team", Version: version, DisplayName: "Research Team", Purpose: "Produce findings",
			Roles:        []kernelteam.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Find evidence", MinimumMembers: 1}},
			Coordination: kernelteam.CoordinationPolicy{Mode: kernelteam.CoordinationDynamic, QuietByDefault: true},
			Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead, ApproverRoleIDs: []string{"researcher"}},
		}
		body, _ := json.Marshal(definition)
		created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/team-definitions", string(body), "")
		if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"digest":"sha256:`) {
			t.Fatalf("definition create = %d %s", created.Code, created.Body.String())
		}
	}
	versions := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/team-definitions/research-team", "", "")
	if versions.Code != http.StatusOK || !strings.Contains(versions.Body.String(), `"version":"2"`) {
		t.Fatalf("definition versions = %d %s", versions.Code, versions.Body.String())
	}

	deployment := &kernelteam.Deployment{
		ID: "research-team-one", Scope: scope, DefinitionID: "research-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
	}
	createPayload, _ := json.Marshal(kernelapi.CreateTeamDeploymentRequest{Deployment: deployment, ActorType: "user", ActorID: "operator", Reason: "initial"})
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/team-deployments", string(createPayload), "")
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"toVersion":"1"`) {
		t.Fatalf("deployment create = %d %s", created.Code, created.Body.String())
	}
	listed := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/team-deployments?scopeKind=workspace&scopeId=local", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"items":[`) ||
		!strings.Contains(listed.Body.String(), `"deployment":{"id":"research-team-one"`) ||
		!strings.Contains(listed.Body.String(), `"definition":{"id":"research-team"`) {
		t.Fatalf("deployment list = %d %s", listed.Code, listed.Body.String())
	}
	foreignList := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/team-deployments?scopeKind=workspace&scopeId=other", "", "")
	if foreignList.Code != http.StatusOK || strings.Contains(foreignList.Body.String(), `research-team-one`) {
		t.Fatalf("cross-scope deployment list = %d %s", foreignList.Code, foreignList.Body.String())
	}
	deployment.Revision = 1
	deployment.Status = kernelteam.DeploymentPaused
	deployment.Roster[0].DisplayName = "Evidence lead"
	updatePayload, _ := json.Marshal(kernelapi.UpdateTeamDeploymentRequest{
		Deployment: deployment, ExpectedRevision: 1, ActorType: "user", ActorID: "operator", Reason: "pause for review",
	})
	updated := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/team-deployments/research-team-one", string(updatePayload), "")
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"status":"paused"`) ||
		!strings.Contains(updated.Body.String(), `"displayName":"Evidence lead"`) || !strings.Contains(updated.Body.String(), `"revision":2`) {
		t.Fatalf("deployment update = %d %s", updated.Code, updated.Body.String())
	}
	stale := performAgentRunRequest(t, server.Handler(), http.MethodPut, "/api/v1/team-deployments/research-team-one", string(updatePayload), "")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale deployment update = %d %s", stale.Code, stale.Body.String())
	}

	activatePayload, _ := json.Marshal(kernelapi.ActivateTeamDefinitionRequest{
		Scope: scope, Version: "2", ExpectedRevision: 2, ActorType: "user", ActorID: "operator", Reason: "reviewed",
	})
	activated := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/team-deployments/research-team-one/activations", string(activatePayload), "")
	if activated.Code != http.StatusOK || !strings.Contains(activated.Body.String(), `"activeVersion":"2"`) || !strings.Contains(activated.Body.String(), `"fromVersion":"1"`) {
		t.Fatalf("deployment activation = %d %s", activated.Code, activated.Body.String())
	}
	wrongScope := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/team-deployments/research-team-one?scopeKind=workspace&scopeId=other", "", "")
	if wrongScope.Code != http.StatusNotFound {
		t.Fatalf("cross-scope deployment = %d %s", wrongScope.Code, wrongScope.Body.String())
	}
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"team-definitions"`) || !strings.Contains(capabilities.Body.String(), `"activate"`) {
		t.Fatalf("capabilities = %d %s", capabilities.Code, capabilities.Body.String())
	}
}
