package openseal

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestPublicEngineComposesFirstClassAgentAndTeamDeployments(t *testing.T) {
	ctx := context.Background()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	agentDefinition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find evidence.",
		Authority: AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "workspace", ID: "local"}
	agentDeployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "local", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "compose Team")
	if err != nil {
		t.Fatal(err)
	}
	teamDefinition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: "research-team", Version: "1", DisplayName: "Research Team", Purpose: "Produce evidence-backed findings",
		Roles:        []TeamRoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Find evidence", MinimumMembers: 1}},
		Coordination: TeamCoordinationPolicy{Mode: TeamCoordinationDynamic, QuietByDefault: true, RequireRoleRelevance: true},
		Approvals:    TeamApprovalPolicy{MaximumRisk: capability.RiskLevelRead, ApproverRoleIDs: []string{"researcher"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	teamDeployment, activation, err := engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: "research-team-one", Scope: scope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Status: TeamDeploymentActive,
		Roster: []TeamRosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
	}, "user", "operator", "initial composition")
	if err != nil || activation.ToVersion != "1" || teamDeployment.Roster[0].AgentDeploymentID != agentDeployment.ID {
		t.Fatalf("Team deployment = %#v, activation = %#v, err = %v", teamDeployment, activation, err)
	}
}

func TestTeamManagementOptionOwnsValidationAndDispatcherComposition(t *testing.T) {
	fallback := ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	engine, err := New(
		WithActionWorkers(ActionWorkerConfig{Scope: Scope{Kind: "tenant", ID: "one"}}, nil, fallback),
		WithTeamManagementActions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.actionValidators) != 2 || len(engine.actionPoolSpecs) != 1 {
		t.Fatalf("Team action wiring validators=%d workers=%d", len(engine.actionValidators), len(engine.actionPoolSpecs))
	}
	// Team Skill authority is the mandatory outer dispatcher for every action
	// pool. WithTeamManagementActions composes Team role mutations immediately
	// inside it; runtime dispatcher tests exercise that inner dispatch path.
	if _, ok := engine.actionPoolSpecs[0].dispatcher.(*runtime.TeamSkillActionDispatcher); !ok {
		t.Fatalf("Team authority was not composed outside Team management: %T", engine.actionPoolSpecs[0].dispatcher)
	}
}
