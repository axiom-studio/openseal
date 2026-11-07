package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func bindingToolActionCatalog(t *testing.T, scope Scope) *skill.Catalog {
	t.Helper()
	ctx := context.Background()
	input := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"tool": map[string]interface{}{"type": "string"}, "hash": map[string]interface{}{"type": "string"},
			"arguments": map[string]interface{}{"type": "object"},
		},
		"required": []interface{}{"tool", "hash", "arguments"},
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Transport: skill.TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &skill.BindingToolsSpec{ModelPrefix: "connector", ReadAction: "read", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]skill.Action{"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input,
			Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"github", "linear"} {
		if err := catalog.Bind(ctx, &skill.Binding{
			ID: slug + "-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
			SkillID: "openseal.connector", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
			Slug: slug, Tools: []skill.BindingTool{{
				Name: "search", Description: "Search " + slug, Effect: skill.BindingToolEffectRead,
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []interface{}{"query"}},
				Arguments:   map[string]interface{}{"tool": "search", "hash": "sha256:" + slug},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return catalog
}
