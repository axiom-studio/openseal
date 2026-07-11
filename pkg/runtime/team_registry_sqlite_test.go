package runtime

import (
	"context"
	"path/filepath"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func TestSQLiteTeamDefinitionsRosterAndActivationsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "teams.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistryWithStore(store)
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find evidence.",
		SkillRequirements: []kernelagent.SkillRequirement{{SkillID: "web"}},
		Authority:         kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, AllowedSkillIDs: []string{"web"}, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "operator", "Team roster")
	if err != nil {
		t.Fatal(err)
	}
	teams := kernelteam.NewRegistryWithStore(store, agents)
	for _, version := range []string{"1", "2"} {
		definition := sqliteTeamDefinition(version)
		if _, err := teams.RegisterDefinition(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	deployment, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "research-team", Scope: scope, DefinitionID: "research-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
	}, "user", "operator", "initial")
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := teams.ActivateDefinition(ctx, scope, deployment.ID, "2", deployment.Revision, "user", "operator", "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedAgents := kernelagent.NewRegistryWithStore(reopened)
	restartedTeams := kernelteam.NewRegistryWithStore(reopened, restartedAgents)
	restored, err := restartedTeams.GetDeployment(ctx, scope, deployment.ID)
	if err != nil || restored.ActiveVersion != updated.ActiveVersion || restored.Revision != 2 || restored.Roster[0].AgentDeploymentID != agentDeployment.ID {
		t.Fatalf("restored Team = %#v, err = %v", restored, err)
	}
	versions, err := restartedTeams.ListDefinitionVersions(ctx, "research-team")
	if err != nil || len(versions) != 2 || versions[1].Digest == "" {
		t.Fatalf("restored versions = %#v, err = %v", versions, err)
	}
	activations, err := restartedTeams.ListActivations(ctx, scope, deployment.ID)
	if err != nil || len(activations) != 2 || activations[1].FromVersion != "1" {
		t.Fatalf("restored activations = %#v, err = %v", activations, err)
	}
	if _, err := restartedTeams.GetDeployment(ctx, capability.ScopeReference{Kind: "tenant", ID: "other"}, deployment.ID); err == nil {
		t.Fatal("cross-scope Team deployment should not be visible")
	}
}

func sqliteTeamDefinition(version string) *kernelteam.Definition {
	return &kernelteam.Definition{
		ID: "research-team", Version: version, DisplayName: "Research Team", Purpose: "Produce evidence-backed findings",
		Roles:        []kernelteam.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Find evidence", MinimumMembers: 1, RequiredSkillIDs: []string{"web"}}},
		Coordination: kernelteam.CoordinationPolicy{Mode: kernelteam.CoordinationDynamic, MaximumSpeakersPerRound: 2, QuietByDefault: true},
		Delegation:   kernelteam.DelegationPolicy{MaximumDepth: 2, MaximumConcurrent: 4, RequireAcceptance: true},
		Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelExternal, ApproverRoleIDs: []string{"researcher"}},
	}
}
