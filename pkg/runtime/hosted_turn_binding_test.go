package runtime

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// Regression: two bindings of one Skill exposed the same capability name and
// hosted turn compilation silently routed every proposal to the first one.
func TestHostedTurnFormRefusesDuplicateCapabilityNames(t *testing.T) {
	actions := []capability.ModelAction{
		{Name: "openseal.presentation.update_visual", BindingID: "bundled:presentation", BindingRevision: 2},
		{Name: "openseal.presentation.update_visual", BindingID: "workforce:presentation", BindingRevision: 1},
	}
	_, err := CompileHostedTurnForm(hostedTurnBindingForm(actions[0].Name, map[string]interface{}{}), actions)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate capability names were compiled: %v", err)
	}
	runner := &HostedTurnRunner{config: HostedTurnRunnerConfig{Actions: actions}}
	if _, err := runner.reuseSucceededAction(TurnAction{Capability: actions[0].Name, BindingID: "bundled:presentation", BindingRevision: 2, InputRef: "/actionInputs/proposed"}, map[string]interface{}{}); err == nil {
		t.Fatal("reuse resolved a duplicated capability name")
	}
}

// Two bindings of one binding-tools Skill have distinct names, and each
// compiled proposal carries its exact binding and fixed tool arguments.
func TestHostedTurnFormRoutesBindingToolsOfTwoBindingsExactly(t *testing.T) {
	tool := func(slug, binding string) capability.ModelAction {
		return capability.ModelAction{
			Name: "connector." + slug + ".search", BindingID: binding, BindingRevision: 3, SkillID: "openseal.connector", Version: "1.0.0", Action: "read",
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []interface{}{"query"}},
			Risk:        capability.RiskLevelRead, SideEffect: capability.SideEffectRead,
			BindingTool: &capability.ModelActionBindingTool{Name: "search", InputArgument: "arguments", Arguments: map[string]interface{}{"tool": "search", "hash": "sha256:" + slug}},
		}
	}
	actions := []capability.ModelAction{tool("github", "github-binding"), tool("linear", "linear-binding")}
	response, err := CompileHostedTurnForm(hostedTurnBindingForm("connector.linear.search", map[string]interface{}{"query": "open issues"}), actions)
	if err != nil {
		t.Fatal(err)
	}
	proposed := response.ProposedAction
	if proposed.BindingID != "linear-binding" || proposed.BindingRevision != 3 {
		t.Fatalf("proposal routed to %s@%d", proposed.BindingID, proposed.BindingRevision)
	}
	selected, err := selectTurnModelAction(actions, *proposed)
	if err != nil || selected == nil || selected.BindingID != "linear-binding" {
		t.Fatalf("selected %#v, %v", selected, err)
	}
	arguments, err := resolveTurnActionInput(response.ContinuationCheckpoint, proposed.InputRef)
	if err != nil {
		t.Fatal(err)
	}
	input := selected.ActionInput(arguments)
	if input["tool"] != "search" || input["hash"] != "sha256:linear" || input["arguments"].(map[string]interface{})["query"] != "open issues" {
		t.Fatalf("base action input = %#v", input)
	}
	if _, err := CompileHostedTurnForm(hostedTurnBindingForm("connector.linear.search", map[string]interface{}{"query": 7}), actions); err == nil {
		t.Fatal("tool input outside its schema was compiled")
	}
}

func hostedTurnBindingForm(capabilityName string, arguments map[string]interface{}) HostedTurnForm {
	return HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion,
		NextRunStatus: AgentRunStatusRunning,
		ProposedAction: &HostedTurnActionForm{
			Capability: capabilityName, Summary: "Search the connected service",
			IdempotencyKey: "search", Arguments: arguments,
		},
	}
}
