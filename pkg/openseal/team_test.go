package openseal

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
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
	seedSpace(t, engine, Scope{Kind: scope.Kind, ID: scope.ID}, "operator")
	agentDeployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "local", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "compose Team")
	if err != nil {
		t.Fatal(err)
	}
	teamDefinition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: "research-team", Version: "1", DisplayName: "Research Team", Purpose: "Produce evidence-backed findings",
		Approvals: TeamApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	teamDeployment, activation, err := engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: "research-team-one", Scope: scope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Status: TeamDeploymentActive,
		Roster: []TeamRosterAssignment{{ID: "researcher", AgentDeploymentID: agentDeployment.ID}},
	}, "user", "operator", "initial composition")
	if err != nil || activation.ToVersion != "1" || teamDeployment.Roster[0].AgentDeploymentID != agentDeployment.ID {
		t.Fatalf("Team deployment = %#v, activation = %#v, err = %v", teamDeployment, activation, err)
	}
}

func TestPublicEngineProjectsEveryTeamBindingToEachMember(t *testing.T) {
	ctx := context.Background()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range []*SkillDefinition{
		{ID: "portfolio", Version: "1", Name: "Portfolio", Transport: SkillTransportReference{Kind: "local"}, Actions: map[string]SkillAction{
			"read":  {Name: "read", Description: "Read a portfolio", Risk: SkillRiskRead, SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}},
			"pause": {Name: "pause", Description: "Pause a portfolio", Risk: SkillRiskWrite, SideEffect: SkillSideEffectWrite, Idempotency: SkillIdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}},
		}},
		{ID: "email", Version: "1", Name: "Email", Transport: SkillTransportReference{Kind: "local"}, Actions: map[string]SkillAction{
			"send": {Name: "send", Description: "Send an email", Risk: SkillRiskExternal, SideEffect: SkillSideEffectExternal, Idempotency: SkillIdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}},
		}},
	} {
		if err := engine.RegisterSkill(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	seedSpace(t, engine, Scope{Kind: scope.Kind, ID: scope.ID}, "operator")
	agentMaximumRisk := SkillRiskExternal
	agentDefinition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate portfolios", SystemPrompt: "Operate safely.",
		SkillRequirements: []AgentSkillRequirement{{SkillID: "portfolio"}, {SkillID: "email"}},
		Authority:         AgentAuthorityPolicy{MaximumRisk: SkillRiskExternal, AllowedSkillIDs: []string{"portfolio", "email"}, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "operator-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "local", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
		Restrictions: AgentDeploymentRestrictions{AllowedSkillIDs: []string{"portfolio", "email"}, MaximumRisk: &agentMaximumRisk},
	}, "user", "operator", "create operator")
	if err != nil {
		t.Fatal(err)
	}
	teamDefinition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: "operations", Version: "1", DisplayName: "Operations", Purpose: "Operate portfolios",
		Approvals: TeamApprovalPolicy{MaximumRisk: SkillRiskExternal},
	})
	if err != nil {
		t.Fatal(err)
	}
	teamDeployment, _, err := engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: "operations-one", Scope: scope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version, Status: TeamDeploymentActive,
		Roster:       []TeamRosterAssignment{{ID: "operator", AgentDeploymentID: agentDeployment.ID}},
		Restrictions: TeamDeploymentRestrictions{AllowedSkillIDs: []string{"portfolio", "email"}, MaximumRisk: SkillRiskExternal},
	}, "user", "operator", "create operations Team")
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []*SkillBinding{
		{ID: "portfolio", Scope: scope, DeploymentID: teamDeployment.ID, SkillID: "portfolio", SkillVersion: "1", AllowedActions: []string{"read", "pause"}, MaximumRisk: SkillRiskWrite, Revision: 1},
		{ID: "email", Scope: scope, DeploymentID: teamDeployment.ID, SkillID: "email", SkillVersion: "1", AllowedActions: []string{"send"}, MaximumRisk: SkillRiskExternal, Revision: 1},
	} {
		if err := engine.BindSkill(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}

	activation, err := engine.ActivateTeamSkillsForAgent(ctx, scope, teamDeployment.ID, agentDeployment.ID, SkillHostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(activation.Skills) != 2 || activation.Skills[0].SkillID != "email" || activation.Skills[1].SkillID != "portfolio" ||
		len(activation.Skills[1].Actions) != 2 || activation.Skills[1].Actions[0].DeploymentID != teamDeployment.ID {
		t.Fatalf("every member gets every Team binding: %#v", activation.Skills)
	}
	if _, err := engine.ActivateTeamSkillsForAgent(ctx, scope, teamDeployment.ID, "not-a-member", SkillHostCapabilityState{}); err == nil {
		t.Fatal("expected a non-member Team activation to fail closed")
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
	if _, ok := engine.actionPoolSpecs[0].dispatcher.(*TeamMemberActionDispatcher); !ok {
		t.Fatalf("Team dispatcher was not composed over the guarded host fallback: %T", engine.actionPoolSpecs[0].dispatcher)
	}
	result, err := engine.actionPoolSpecs[0].dispatcher.DispatchAction(context.Background(), ActionDispatchInput{})
	if err != nil || result["fallback"] != true {
		t.Fatalf("Team dispatcher host fallback = %#v, %v", result, err)
	}
}
