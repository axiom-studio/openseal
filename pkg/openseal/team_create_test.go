package openseal

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestTeamCreateEngineWiring(t *testing.T) {
	fallback := ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	engine, err := New(
		WithActionWorkers(ActionWorkerConfig{Scope: Scope{Kind: "tenant", ID: "one"}}, nil, fallback),
		WithAgentManagementActions(),
		WithTeamManagementActions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	validators := 0
	for _, validator := range engine.actionValidators {
		if _, ok := validator.(*agentbuilder.TeamCreateActionValidator); ok {
			validators++
		}
	}
	if validators != 1 {
		t.Fatalf("create_team validators = %d", validators)
	}
	definition, err := engine.skills.GetDefinition(context.Background(), TeamManagementSkillID, TeamManagementSkillVersion)
	if err != nil || definition.Actions[TeamActionCreateTeam].Name != TeamActionCreateTeam {
		t.Fatalf("openseal.teams %s = %#v, %v", TeamManagementSkillVersion, definition, err)
	}
	// create_team passes the Team role dispatcher and reaches the builder
	// dispatcher, which refuses a call-less input instead of the host fallback.
	bound := &skill.BoundAction{Definition: definition, Action: definition.Actions[TeamActionCreateTeam]}
	result, err := engine.actionPoolSpecs[0].dispatcher.DispatchAction(context.Background(), ActionDispatchInput{Bound: bound})
	if err == nil || result["fallback"] == true {
		t.Fatalf("create_team reached the host fallback: %#v, %v", result, err)
	}
}
