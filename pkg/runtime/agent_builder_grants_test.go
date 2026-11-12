// In-package checks of the builder action contract and of how reviewed
// grants set an Agent's risk ceiling.

package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestAgentBuilderActionsExistOnlyInCurrentContract(t *testing.T) {
	definition := models.AgentManagementSkill()
	if definition.Version != "1.6.0" {
		t.Fatalf("version = %s", definition.Version)
	}
	for _, name := range []string{models.AgentActionListAgents, models.AgentActionCreateAgent, models.AgentActionUpdateAgent, models.AgentActionTryAgent} {
		action, ok := definition.Actions[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if !isAgentBuilderAction(&skill.BoundAction{Definition: definition, Action: action}) {
			t.Fatalf("%s is not a builder action at %s", name, definition.Version)
		}
		older := models.AgentManagementSkill()
		older.Version = "1.5.0"
		if isAgentBuilderAction(&skill.BoundAction{Definition: older, Action: action}) {
			t.Fatalf("%s matched the 1.5.0 contract", name)
		}
		if models.IsAgentManagementAction(&skill.BoundAction{Definition: definition, Action: action}) {
			t.Fatalf("%s reached the self-only behavior dispatcher", name)
		}
		wantReview := name == models.AgentActionCreateAgent || name == models.AgentActionUpdateAgent
		if action.AlwaysReview() != wantReview {
			t.Fatalf("%s review always = %v", name, action.AlwaysReview())
		}
	}
	if !models.IsSupportedAgentManagementSkillVersion("1.4.0") {
		t.Fatal("1.4.0 bindings are no longer supported")
	}
}

// A grant's declared ceiling is never trusted: it becomes the riskiest
// allowed action of the exact catalog definition, at least read.
func TestResolveAgentSkillGrantsRiskFromCatalog(t *testing.T) {
	ctx := context.Background()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "acme.lookup", Version: "1.0.0", Name: "Lookup", Description: "Look things up",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://lookup"},
		Prompt:    &skill.PromptModule{Instructions: "Look things up."},
		Actions: map[string]skill.Action{
			"find": {
				Name: "find", Description: "Find a thing", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
				InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
			},
			"save": {
				Name: "save", Description: "Save a thing", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
				InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		actions  []string
		prompt   bool
		declared skill.RiskLevel
		want     skill.RiskLevel
	}{
		{"declared destructive on a read action", []string{"find"}, false, skill.RiskLevelDestructive, skill.RiskLevelRead},
		{"declared read on a write action", []string{"find", "save"}, false, skill.RiskLevelRead, skill.RiskLevelWrite},
		{"prompt only", nil, true, skill.RiskLevelProduction, skill.RiskLevelRead},
	} {
		t.Run(test.name, func(t *testing.T) {
			grants, err := resolveAgentSkillGrants(ctx, catalog, []models.AgentSkillGrant{{
				SkillID: "acme.lookup", SkillVersion: "1.0.0", AllowedActions: test.actions, EnablePrompt: test.prompt, MaximumRisk: test.declared,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := grants[0].grant.MaximumRisk; got != test.want {
				t.Fatalf("grant risk = %s, want %s", got, test.want)
			}
			if got := agentGrantsMaximumRisk("", grants); got != test.want {
				t.Fatalf("agent ceiling = %s, want %s", got, test.want)
			}
		})
	}
}
