package team

import (
	"context"
	"errors"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestPrepareAgentRemovalKeepsOneMember(t *testing.T) {
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
	if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &Deployment{
		ID: "market-team", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: DeploymentActive,
		Roster: []RosterAssignment{
			{ID: "one", AgentDeploymentID: "ana"},
			{ID: "two", AgentDeploymentID: "ben"},
			{ID: "three", AgentDeploymentID: "cy"},
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
	// Nothing is stored yet.
	change, err := prepare("cy")
	if err != nil || len(change.Deployment.Roster) != 2 || change.Deployment.Revision != 2 || change.ExpectedRevision != 1 ||
		change.Deployment.ActiveVersion != definition.Version {
		t.Fatalf("change = %+v, err = %v", change, err)
	}
	if stored, err := registry.GetDeployment(ctx, scope, "market-team"); err != nil || len(stored.Roster) != 3 {
		t.Fatalf("prepared change was stored: %+v, %v", stored, err)
	}
	for _, agentID := range []string{"cy", "ben"} {
		change, err := prepare(agentID)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.store.UpdateTeamDeployment(ctx, change.Deployment, change.ExpectedRevision, change.Activation); err != nil {
			t.Fatal(err)
		}
	}
	if versions, err := registry.ListDefinitionVersions(ctx, definition.ID); err != nil || len(versions) != 1 {
		t.Fatalf("removing members registered a definition: %d, %v", len(versions), err)
	}
	if _, err := prepare("ana"); !errors.Is(err, ErrRosterMinimum) {
		t.Fatalf("removing the last member: %v", err)
	}
}
