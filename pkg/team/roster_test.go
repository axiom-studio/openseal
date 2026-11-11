package team

import (
	"context"
	"errors"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestPrepareAgentRemovalKeepsRoleMinimums(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agents := kernelagent.NewRegistry()
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find evidence.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ana", "ben", "cy"} {
		if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
			ID: id, Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "create"); err != nil {
			t.Fatal(err)
		}
	}
	registry := NewRegistry(agents)
	definition := validDefinition()
	definition.Roles[0].MinimumMembers = 2
	if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &Deployment{
		ID: "market-team", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: DeploymentActive,
		Roster: []RosterAssignment{
			{ID: "one", RoleID: "researcher", AgentDeploymentID: "ana"},
			{ID: "two", RoleID: "researcher", AgentDeploymentID: "ben"},
			{ID: "three", RoleID: "researcher", AgentDeploymentID: "cy"},
		},
	}, "user", "operator", "create"); err != nil {
		t.Fatal(err)
	}
	prepare := func(agentID string) (*RosterChange, error) {
		return registry.PrepareAgentRemoval(ctx, scope, "market-team", agentID, "user", "operator", "left")
	}
	if change, err := prepare("nobody"); err != nil || change != nil {
		t.Fatalf("an Agent off the roster: %+v, %v", change, err)
	}
	// The role is still held, so the definition stays; nothing is stored yet.
	change, err := prepare("cy")
	if err != nil || change.Definition != nil || len(change.Deployment.Roster) != 2 || change.Deployment.Revision != 2 || change.ExpectedRevision != 1 {
		t.Fatalf("change = %+v, err = %v", change, err)
	}
	if stored, err := registry.GetDeployment(ctx, scope, "market-team"); err != nil || len(stored.Roster) != 3 {
		t.Fatalf("prepared change was stored: %+v, %v", stored, err)
	}
	if err := registry.store.UpdateTeamDeployment(ctx, change.Deployment, change.ExpectedRevision, change.Activation); err != nil {
		t.Fatal(err)
	}
	// Two researchers are the role's minimum.
	if _, err := prepare("ben"); !errors.Is(err, ErrRosterMinimum) {
		t.Fatalf("removal below the role minimum: %v", err)
	}
}
