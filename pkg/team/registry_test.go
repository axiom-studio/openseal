package team

import (
	"context"
	"errors"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestRegistryComposesScopedAgentDeploymentsAndActivatesImmutableVersions(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistry()
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find and cite evidence.",
		SkillRequirements: []kernelagent.SkillRequirement{{SkillID: "web-research"}},
		Authority:         kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, AllowedSkillIDs: []string{"web-research"}, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "operator", "compose Team")
	if err != nil {
		t.Fatal(err)
	}

	registry := NewRegistry(agents)
	definition := validDefinition()
	definition.Roles[0].RequiredSkillIDs = []string{"web-research"}
	registered, err := registry.RegisterDefinition(ctx, definition)
	if err != nil || registered.Digest == "" {
		t.Fatalf("registered definition = %#v, err = %v", registered, err)
	}
	if _, err := registry.RegisterDefinition(ctx, definition); err == nil {
		t.Fatal("Team definition versions must be immutable")
	}
	deployment, activation, err := registry.CreateDeployment(ctx, &Deployment{
		ID: "market-team", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		Roster:       []RosterAssignment{{ID: "primary-researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
		Restrictions: DeploymentRestrictions{MaximumRisk: capability.RiskLevelWrite, MaximumConcurrency: 2}, Status: DeploymentActive,
	}, "user", "operator", "initial Team composition")
	if err != nil || deployment.Revision != 1 || activation.ToVersion != "1.0.0" {
		t.Fatalf("created deployment = %#v, activation = %#v, err = %v", deployment, activation, err)
	}
	if _, err := registry.GetDeployment(ctx, capability.ScopeReference{Kind: "tenant", ID: "other"}, deployment.ID); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("cross-scope deployment lookup err = %v", err)
	}
	proposed := cloneDeployment(deployment)
	proposed.Status = DeploymentPaused
	proposed.Roster[0].DisplayName = "Evidence lead"
	if _, _, err := registry.UpdateDeployment(ctx, proposed, 99, "user", "operator", "pause for review"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale deployment update err = %v", err)
	}
	updatedComposition, compositionActivation, err := registry.UpdateDeployment(ctx, proposed, deployment.Revision, "user", "operator", "pause for review")
	if err != nil || updatedComposition.Status != DeploymentPaused || updatedComposition.Roster[0].DisplayName != "Evidence lead" ||
		updatedComposition.Revision != 2 || compositionActivation.FromVersion != registered.Version || compositionActivation.ToVersion != registered.Version {
		t.Fatalf("updated composition = %#v, activation = %#v, err = %v", updatedComposition, compositionActivation, err)
	}
	deployment = updatedComposition

	next := validDefinition()
	next.Version = "1.1.0"
	next.OperatingPrinciples = []string{"Preserve evidence provenance"}
	if _, err := registry.RegisterDefinition(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ActivateDefinition(ctx, scope, deployment.ID, next.Version, 99, "user", "operator", "stale"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale activation err = %v", err)
	}
	updated, nextActivation, err := registry.ActivateDefinition(ctx, scope, deployment.ID, next.Version, deployment.Revision, "user", "operator", "reviewed")
	if err != nil || updated.ActiveVersion != "1.1.0" || updated.Revision != 3 || nextActivation.FromVersion != "1.0.0" {
		t.Fatalf("updated = %#v, activation = %#v, err = %v", updated, nextActivation, err)
	}
	activations, err := registry.ListActivations(ctx, scope, deployment.ID)
	if err != nil || len(activations) != 3 || activations[1].DeploymentRevision != 2 || activations[2].DeploymentRevision != 3 {
		t.Fatalf("activations = %#v, err = %v", activations, err)
	}
}

func TestRegistryRejectsMissingOrUnderqualifiedRosterAgentsAndAuthorityWidening(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistry()
	registry := NewRegistry(agents)
	definition := validDefinition()
	definition.Roles[0].RequiredSkillIDs = []string{"web-research"}
	registered, err := registry.RegisterDefinition(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	base := &Deployment{
		ID: "market-team", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		Roster: []RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: "missing"}}, Status: DeploymentActive,
	}
	if _, _, err := registry.CreateDeployment(ctx, base, "user", "operator", ""); err == nil {
		t.Fatal("missing Agent deployment should fail closed")
	}
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "writer", Version: "1", DisplayName: "Writer", Purpose: "Write summaries", SystemPrompt: "Write concise summaries.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "writer-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "test")
	if err != nil {
		t.Fatal(err)
	}
	base.Roster[0].AgentDeploymentID = agentDeployment.ID
	if _, _, err := registry.CreateDeployment(ctx, base, "user", "operator", ""); err == nil {
		t.Fatal("underqualified Agent deployment should fail closed")
	}
	definition.Version = "1.0.1"
	definition.Roles[0].RequiredSkillIDs = nil
	registered, err = registry.RegisterDefinition(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	base.DefinitionID, base.ActiveVersion = registered.ID, registered.Version
	base.Restrictions.MaximumRisk = capability.RiskLevelProduction
	if _, _, err := registry.CreateDeployment(ctx, base, "user", "operator", ""); err == nil {
		t.Fatal("authority widening should fail closed")
	}
}
