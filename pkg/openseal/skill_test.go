package openseal

import (
	"context"
	"testing"
)

func TestEngineExposesGovernedSkillCatalog(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	definition := &SkillDefinition{
		ID: "analytics", Version: "1.0.0", Name: "Analytics",
		Transport: SkillTransportReference{Kind: "local"},
		Actions: map[string]SkillAction{
			"query": {
				Name: "query", Description: "Query analytics", Risk: SkillRiskRead,
				SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencySupported,
				InputSchema: map[string]interface{}{
					"type": "object", "properties": map[string]interface{}{"metric": map[string]interface{}{"type": "string"}},
					"required": []interface{}{"metric"},
				},
			},
		},
	}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "local", ID: "test"}
	if err := engine.BindSkill(ctx, &SkillBinding{
		ID: "analytics-binding", Scope: scope, DeploymentID: "analyst", SkillID: "analytics", SkillVersion: "1.0.0",
		AllowedActions: []string{"query"}, MaximumRisk: SkillRiskRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	actions, err := engine.ListModelSkillActions(ctx, scope, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Name != "analytics.query" {
		t.Fatalf("unexpected actions: %#v", actions)
	}
}
