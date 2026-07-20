package runtime

import (
	"context"
	"path/filepath"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
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
	if _, err := teams.RegisterDefinition(ctx, sqliteTeamDefinition("1")); err != nil {
		t.Fatal(err)
	}
	deployment, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "research-team", Scope: scope, DefinitionID: "research-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
	}, "user", "operator", "initial")
	if err != nil {
		t.Fatal(err)
	}
	candidate := sqliteTeamDefinition("2")
	candidate.Purpose = "Produce reviewed evidence-backed findings"
	amendment, err := teams.ProposeAmendment(ctx, kernelteam.ProposeAmendmentRequest{
		Scope: scope, DeploymentID: deployment.ID, Candidate: candidate, ProposerType: "user", ProposerID: "operator", Rationale: "Require reviewed findings",
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := teams.ResolveAmendment(ctx, kernelteam.ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Approved: true, ActorType: "user", ActorID: "operator", Reason: "reviewed",
	})
	if err != nil {
		t.Fatal(err)
	}
	activated, updated, _, err := teams.ActivateAmendment(ctx, scope, amendment.ID, approved.Revision, "user", "operator", "approved")
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
	restoredAmendment, err := restartedTeams.GetAmendment(ctx, scope, amendment.ID)
	if err != nil || restoredAmendment.Status != kernelteam.AmendmentActivated || restoredAmendment.ActivationID != activated.ActivationID {
		t.Fatalf("restored Team amendment = %#v, err = %v", restoredAmendment, err)
	}
	restoredAmendments, err := restartedTeams.ListAmendments(ctx, scope, deployment.ID)
	if err != nil || len(restoredAmendments) != 1 || restoredAmendments[0].ID != amendment.ID {
		t.Fatalf("restored Team amendment list = %#v, err = %v", restoredAmendments, err)
	}
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
		Amendments: workforce.AmendmentPolicy{
			AllowedFields: []string{"purpose"}, RequiresApproval: true, ApproverPrincipals: []string{"user:operator"},
		},
	}
}
