package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
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

// Regression: with two bindings of one Skill, the materialized ActionCall
// belongs to the binding whose tool the model named, with that binding's
// fixed arguments, and a direct proposal cannot reach an undeclared tool.
func TestBindingToolMaterializesAgainstItsExactBinding(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := bindingToolActionCatalog(t, scope)
	claimed := claimedActionRun(t, store, scope, time.Now().UTC(), "worker")
	modelActions, err := catalog.ListModelActions(t.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-agent")
	if err != nil || len(modelActions) != 2 {
		t.Fatalf("model actions = %#v, %v", modelActions, err)
	}
	actions := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "read-only"}, nil
	}))
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, errors.New("not needed")
	}), nil, AgentRunWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	pool.SetActionCoordinator(actions)
	_, err = actions.Propose(t.Context(), ProposeActionRequest{
		Scope: scope, RunID: claimed.ID, WorkerID: "worker", DeploymentID: "release-agent", BindingID: "github-binding", BindingRevision: 1,
		SkillID: "openseal.connector", SkillVersion: "1.0.0", Action: "read", IdempotencyKey: "direct", Summary: "Direct",
		Arguments: map[string]interface{}{"tool": "search", "hash": "sha256:linear", "arguments": map[string]interface{}{"query": "open"}},
		Actor:     ActivityActor{Type: "worker", ID: "worker"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not select a tool declared by its binding") {
		t.Fatalf("a direct proposal reached a tool pinned by another binding: %v", err)
	}
	turn := &AgentTurn{ID: "tool-turn", RequestedActions: []TurnAction{{Type: "skill_action", Capability: "connector.linear.search", Summary: "Search Linear", IdempotencyKey: "search", InputRef: "/inputs/search"}},
		ContinuationCheckpoint: map[string]interface{}{"inputs": map[string]interface{}{"search": map[string]interface{}{"query": "open"}}}}
	if _, err := pool.materializeTurnAction(t.Context(), "worker", claimed, turn, &TurnRunnerBinding{DeploymentID: "release-agent", ModelActions: modelActions}); err != nil {
		t.Fatal(err)
	}
	calls, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: scope, RunID: claimed.ID})
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls = %#v, %v", calls, err)
	}
	call := calls[0]
	nested, _ := call.Arguments["arguments"].(map[string]interface{})
	if call.BindingID != "linear-binding" || call.Action != "read" || call.Arguments["tool"] != "search" || call.Arguments["hash"] != "sha256:linear" || nested["query"] != "open" {
		t.Fatalf("call = %#v", call)
	}

}
