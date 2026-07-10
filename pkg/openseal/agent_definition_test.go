package openseal

import (
	"context"
	"testing"
)

func TestEngineExposesVersionedAgentDefinitionLifecycle(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	definition := func(version string) *AgentDefinition {
		return &AgentDefinition{
			ID: "marketer", Version: version, DisplayName: "Marketer", Purpose: "Turn product work into demand",
			SystemPrompt:      "Create accurate, useful marketing material and follow up respectfully.",
			SkillRequirements: []AgentSkillRequirement{{SkillID: "publisher"}},
			Authority:         AgentAuthorityPolicy{MaximumRisk: SkillRiskExternal, AllowedSkillIDs: []string{"publisher"}, MaxConcurrentRuns: 3},
			Amendments:        AgentAmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}, RequiresApproval: true},
		}
	}
	ctx := context.Background()
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if _, err := engine.RegisterAgentDefinition(ctx, definition(version)); err != nil {
			t.Fatal(err)
		}
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	deployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "marketing-prod", Scope: scope, DefinitionID: "marketer", ActiveVersion: "1.0.0",
		RolloutStatus: AgentRolloutActive, Environment: "production", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	deployed, activation, err := engine.ActivateAgentDefinition(ctx, scope, deployment.ID, "1.1.0", deployment.Revision, "user", "admin", "evaluation passed")
	if err != nil || deployed.ActiveVersion != "1.1.0" || activation.FromVersion != "1.0.0" {
		t.Fatalf("public rollout = %#v %#v, %v", deployed, activation, err)
	}
	history, err := engine.ListAgentDefinitionActivations(ctx, scope, deployment.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("public activation history = %#v, %v", history, err)
	}
}
