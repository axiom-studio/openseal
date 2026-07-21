package openseal

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestSkillManagementOptionOwnsDefinitionValidationAndDispatcherComposition(t *testing.T) {
	fallback := ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	engine, err := New(
		WithActionWorkers(ActionWorkerConfig{Scope: Scope{Kind: "tenant", ID: "one"}}, nil, fallback),
		WithSkillManagementActions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := engine.GetSkillDefinition(context.Background(), SkillManagementSkillID, SkillManagementSkillVersion)
	if err != nil || definition == nil || definition.Version != "1.1.0" ||
		definition.Actions[SkillActionDiscover].Name != SkillActionDiscover ||
		definition.Actions[SkillActionUpsertBinding].Name != SkillActionUpsertBinding {
		t.Fatalf("built-in Skill management definition = %#v, %v", definition, err)
	}
	if len(engine.actionValidators) != 2 || len(engine.actionPoolSpecs) != 1 {
		t.Fatalf("Skill action wiring validators=%d workers=%d", len(engine.actionValidators), len(engine.actionPoolSpecs))
	}
	// Team authority is the mandatory outer dispatcher for every action pool;
	// the Skill dispatcher is composed immediately before it and is exercised
	// directly by the runtime package tests.
	if _, ok := engine.actionPoolSpecs[0].dispatcher.(*runtime.TeamSkillActionDispatcher); !ok {
		t.Fatalf("Team authority was not composed outside Skill management: %T", engine.actionPoolSpecs[0].dispatcher)
	}
}
