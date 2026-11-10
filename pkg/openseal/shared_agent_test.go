package openseal

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSharedAgentEngineInstallsAndExports(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	engine, err := New(WithAgentManagementActions(), WithAgentBuilderHost(agentBuilderEngineHost{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RegisterSkill(ctx, &skill.Definition{
		ID: "acme.search", Version: "1.0.0", Name: "Search", Description: "Search the web",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://search"},
		Actions: map[string]skill.Action{"search": {
			Name: "search", Description: "Search", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
			Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
			InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	request := SharedAgentInstallRequest{
		Scope: scope, ActingUserID: "person-2", IdempotencyKey: "adoption-1",
		Source:  SharedAgentSource{ShareID: "sa1", Version: 1},
		Profile: SharedAgentProfile{DisplayName: "Scout", Purpose: "Find things.", SystemPrompt: "Find things on the web."},
		SpaceID: runtime.DefaultSpaceID(scope), ExpectedMaximumRisk: skill.RiskLevelRead,
		Grants: []SharedAgentSkill{{SkillID: "acme.search", SkillVersion: "1.0.0", AllowedActions: []string{"search"}}},
	}
	plan, err := engine.PlanSharedAgentInstall(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := engine.InstallSharedAgent(ctx, plan)
	if err != nil || installed.ConversationID == "" || len(installed.Bindings) != 1 {
		t.Fatalf("installed = %#v, %v", installed, err)
	}
	reference := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	if _, err := engine.ExportSharedAgent(ctx, reference, installed.AgentID); !errors.Is(err, ErrSharedAgentIsCopy) {
		t.Fatalf("export of the copy = %v", err)
	}
	if err := engine.RetireSharedAgentInstall(ctx, reference, installed.AgentID, "person-2"); err != nil {
		t.Fatal(err)
	}

	bare, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.ExportSharedAgent(ctx, reference, "any"); !errors.Is(err, errSharedAgentsUnavailable) {
		t.Fatalf("export without the builder actions = %v", err)
	}
}
