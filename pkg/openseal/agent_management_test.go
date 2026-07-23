package openseal

import (
	"context"
	"testing"
)

func TestAgentManagementOptionOwnsDefinitionValidationAndDispatcherComposition(t *testing.T) {
	fallback := ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	engine, err := New(
		WithActionWorkers(ActionWorkerConfig{Scope: Scope{Kind: "tenant", ID: "one"}}, nil, fallback),
		WithAgentManagementActions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := engine.GetSkillDefinition(context.Background(), AgentManagementSkillID, AgentManagementSkillVersion)
	if err != nil || definition == nil || definition.Version != AgentManagementSkillVersion ||
		definition.Actions[AgentActionAmendBehavior].Name != AgentActionAmendBehavior {
		t.Fatalf("built-in Agent management definition = %#v, %v", definition, err)
	}
	if len(engine.actionValidators) != 2 || len(engine.actionPoolSpecs) != 1 {
		t.Fatalf("Agent action wiring validators=%d workers=%d", len(engine.actionValidators), len(engine.actionPoolSpecs))
	}
	if _, ok := engine.actionPoolSpecs[0].dispatcher.(*AgentBehaviorActionDispatcher); !ok {
		t.Fatalf("Agent dispatcher was not composed over the guarded host fallback: %T", engine.actionPoolSpecs[0].dispatcher)
	}
	result, err := engine.actionPoolSpecs[0].dispatcher.DispatchAction(context.Background(), ActionDispatchInput{})
	if err != nil || result["fallback"] != true {
		t.Fatalf("Agent dispatcher host fallback = %#v, %v", result, err)
	}
}
