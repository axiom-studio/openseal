package runtime

import (
	"reflect"
	"strings"
	"testing"

	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// withBindingTools binds a connector Skill whose tools come from two
// bindings, github and linear, to the routine's Agent.
func (f *routineGrantFixtureState) withBindingTools(t *testing.T) {
	t.Helper()
	input := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"tool": map[string]interface{}{"type": "string"}, "hash": map[string]interface{}{"type": "string"},
			"arguments": map[string]interface{}{"type": "object"},
		},
		"required": []interface{}{"tool", "hash", "arguments"},
	}
	if err := f.catalog.Register(t.Context(), &skill.Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Transport: skill.TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &skill.BindingToolsSpec{ModelPrefix: "mcp", ReadAction: "read", WriteAction: "call", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]skill.Action{
			"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported},
			"call": {Name: "call", Description: "Call a tool", InputSchema: input, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
		},
	}); err != nil {
		t.Fatal(err)
	}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"title": map[string]interface{}{"type": "string"}}}
	tool := func(slug, name, description string, effect skill.BindingToolEffect) skill.BindingTool {
		return skill.BindingTool{Name: name, Description: description, Effect: effect, InputSchema: schema, Arguments: map[string]interface{}{"tool": name, "hash": slug + "-" + name}}
	}
	for slug, tools := range map[string][]skill.BindingTool{
		"github": {
			tool("github", "create_issue", "Create a GitHub issue. Returns its number.", skill.BindingToolEffectWrite),
			tool("github", "close_issue", "Close a GitHub issue", skill.BindingToolEffectWrite),
			tool("github", "search", "Search GitHub issues", skill.BindingToolEffectRead),
		},
		"linear": {tool("linear", "create_issue", "Create a Linear issue", skill.BindingToolEffectWrite)},
	} {
		if err := f.catalog.Bind(t.Context(), &skill.Binding{
			ID: slug + "-binding", Revision: 1, Scope: f.bound.Binding.Scope, DeploymentID: f.run.AssignedAgentID, SkillID: "openseal.connector", SkillVersion: "1.0.0",
			AllowedActions: []string{"read", "call"}, MaximumRisk: skill.RiskLevelWrite, Slug: slug, Tools: tools,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *routineGrantFixtureState) proposeTool(t *testing.T, coordinator *runtimeaction.ActionCoordinator, run *AgentRun, slug, action, tool, key string) *ActionProposalResult {
	t.Helper()
	return f.proposeBindingTool(t, coordinator, run, slug+"-binding", 1, action, tool, slug+"-"+tool, key)
}

func (f *routineGrantFixtureState) proposeBindingTool(t *testing.T, coordinator *runtimeaction.ActionCoordinator, run *AgentRun, bindingID string, revision int64, action, tool, hash, key string) *ActionProposalResult {
	t.Helper()
	result, err := coordinator.Propose(t.Context(), runtimeaction.ProposeActionRequest{
		Scope: run.Scope, RunID: run.ID, TurnID: "step-1", WorkerID: "worker", Origin: ActionOriginLoop, Execution: ActionExecutionLoop, Summary: "Run the routine's step.", DeploymentID: run.AssignedAgentID, BindingID: bindingID, BindingRevision: revision,
		SkillID: "openseal.connector", SkillVersion: "1.0.0", Action: action, IdempotencyKey: key,
		Arguments: map[string]interface{}{"tool": tool, "hash": hash, "arguments": map[string]interface{}{"title": "Forecast"}},
	})
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return result
}

func TestRoutineBriefGrantsBindingToolsIndividually(t *testing.T) {
	f := routineGrantFixture(t)
	f.withBindingTools(t)
	for name, tc := range map[string]struct {
		action, err string
	}{
		"write base action": {"openseal.connector.call", "grant the specific tool, e.g. mcp."},
		"read base action":  {"openseal.connector.read", "grant the specific tool, e.g. mcp.github.search"},
		"undeclared tool":   {"mcp.github.delete_repo", "is not an action this Agent can use"},
		"unknown binding":   {"mcp.gitlab.create_issue", "is not an action this Agent can use"},
	} {
		if _, err := f.resolve(t, f.withGrants(map[string]interface{}{"action": tc.action})); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.err)
		}
	}
	resolved, err := f.resolve(t, f.withGrants(
		map[string]interface{}{"action": "mcp.github.create_issue", "label": "mcp.github.create_issue", "toolArguments": map[string]interface{}{"tool": "create_issue", "hash": "forged"}},
		map[string]interface{}{"action": "mcp.github.search"},
		map[string]interface{}{"action": "mail.note", "toolArguments": map[string]interface{}{"tool": "note"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	grants := resolved["brief"].(map[string]interface{})["grants"].([]interface{})
	if got := grants[0].(map[string]interface{})["label"]; got != "Create a GitHub issue" {
		t.Fatalf("binding tool label = %v, want the tool's description", got)
	}
	if got := grants[1].(map[string]interface{})["label"]; got != "Search GitHub issues" {
		t.Fatalf("binding read tool label = %v", got)
	}
	// The pin always comes from the catalog: a model-supplied one is replaced,
	// and a definition action carries none.
	if got := grants[0].(map[string]interface{})["toolArguments"]; !reflect.DeepEqual(got, map[string]interface{}{"tool": "create_issue", "hash": "github-create_issue"}) {
		t.Fatalf("binding tool pin = %#v", got)
	}
	if got, ok := grants[2].(map[string]interface{})["toolArguments"]; ok {
		t.Fatalf("definition action pin = %#v", got)
	}
	if err := f.catalog.ValidateInput(t.Context(), f.bound, resolved); err != nil {
		t.Fatalf("pinned brief breaks the create_task contract: %v", err)
	}

}

// Regression: every binding tool routes to one base action, so a grant must
// cover exactly the tool it names on its own binding and nothing else.
func TestRoutineGrantCoversOnlyTheGrantedBindingTool(t *testing.T) {
	f := routineGrantFixture(t)
	f.withBindingTools(t)
	activation, _ := f.createRoutine(t, map[string]interface{}{"action": "mcp.github.create_issue"})
	f.reviewEverything(t, activation)
	coordinator := runtimeaction.NewActionCoordinator(f.store, f.store, f.catalog, runtimeaction.NewDefaultActionPolicy(), nil)

	cases := []struct {
		name, slug, action, tool string
		asks                     bool
	}{
		{"granted tool", "github", "call", "create_issue", false},
		{"other write tool on the binding", "github", "call", "close_issue", true},
		{"same tool on the other binding", "linear", "call", "create_issue", true},
		{"read tool", "github", "read", "search", false},
	}
	for index, tc := range cases {
		run := f.routineOccurrence(t, activation, "tool-occurrence-"+tc.name)
		result := f.proposeTool(t, coordinator, run, tc.slug, tc.action, tc.tool, "tool-call-"+string(rune('a'+index)))
		if asks := result.Approval != nil; asks != tc.asks {
			t.Fatalf("%s: asks=%v want %v (call %#v)", tc.name, asks, tc.asks, result.Call)
		}
	}

	// A grant naming the base action, however it was stored, covers no tool.
	stored, err := f.store.GetRunbookActivation(t.Context(), f.run.Scope, activation.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneRunbookActivation(stored)
	next.Task.Grants = []RoutineGrant{{Action: "openseal.connector.call", Label: "Use connected tools"}}
	next.Revision++
	if err := f.store.UpdateRunbookActivation(t.Context(), next, stored.Revision); err != nil {
		t.Fatal(err)
	}
	for index, slug := range []string{"github", "linear"} {
		run := f.routineOccurrence(t, next, "base-occurrence-"+slug)
		if result := f.proposeTool(t, coordinator, run, slug, "call", "create_issue", "base-call-"+string(rune('a'+index))); result.Approval == nil {
			t.Fatalf("%s: a base action grant ran a binding tool without asking", slug)
		}
	}
}

// A grant is pinned to the exact tool the user approved. A binding recreated
// under the same slug for another server, or a tool re-pinned to a new hash
// after a reconnect, has the same model name but no longer runs unasked.
func TestRoutineGrantPinsTheApprovedTool(t *testing.T) {
	f := routineGrantFixture(t)
	f.withBindingTools(t)
	activation, _ := f.createRoutine(t, map[string]interface{}{"action": "mcp.github.create_issue"})
	f.reviewEverything(t, activation)
	if pin := activation.Task.Grants[0].ToolArguments; !reflect.DeepEqual(pin, map[string]interface{}{"tool": "create_issue", "hash": "github-create_issue"}) {
		t.Fatalf("stored pin = %#v", pin)
	}
	coordinator := runtimeaction.NewActionCoordinator(f.store, f.store, f.catalog, runtimeaction.NewDefaultActionPolicy(), nil)
	if result := f.proposeTool(t, coordinator, f.routineOccurrence(t, activation, "pinned"), "github", "call", "create_issue", "pinned"); result.Approval != nil {
		t.Fatal("the approved tool asked")
	}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"title": map[string]interface{}{"type": "string"}}}
	rebind := func(id string, revision int64, hash string) {
		t.Helper()
		if err := f.catalog.Bind(t.Context(), &skill.Binding{
			ID: id, Revision: revision, Scope: f.bound.Binding.Scope, DeploymentID: f.run.AssignedAgentID, SkillID: "openseal.connector", SkillVersion: "1.0.0",
			AllowedActions: []string{"read", "call"}, MaximumRisk: skill.RiskLevelWrite, Slug: "github",
			Tools: []skill.BindingTool{{Name: "create_issue", Description: "Create a GitHub issue", Effect: skill.BindingToolEffectWrite, InputSchema: schema, Arguments: map[string]interface{}{"tool": "create_issue", "hash": hash}}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Reconnect re-pins the tool to a new hash: the routine asks again.
	rebind("github-binding", 2, "github-create_issue-v2")
	if result := f.proposeBindingTool(t, coordinator, f.routineOccurrence(t, activation, "repinned"), "github-binding", 2, "call", "create_issue", "github-create_issue-v2", "repinned"); result.Approval == nil {
		t.Fatal("a re-pinned tool ran under the old approval")
	}

	// The slug is reused by a new binding for another server.
	if _, err := f.catalog.DeleteBinding(t.Context(), skill.DeleteBindingRequest{Scope: f.bound.Binding.Scope, DeploymentID: f.run.AssignedAgentID, BindingID: "github-binding", ExpectedRevision: 2, Actor: skill.BindingActor{Type: "user", ID: "user"}, Reason: "reconnect"}); err != nil {
		t.Fatal(err)
	}
	rebind("github-other", 1, "other-server-create_issue")
	if result := f.proposeBindingTool(t, coordinator, f.routineOccurrence(t, activation, "reused-slug"), "github-other", 1, "call", "create_issue", "other-server-create_issue", "reused-slug"); result.Approval == nil {
		t.Fatal("a tool of another server under the reused slug ran under the old approval")
	}
}

func TestRoutineBindingToolGrantInstruction(t *testing.T) {
	if instruction := routine.BindingToolGrantInstruction([]skill.ModelAction{{Name: "mail.send", SkillID: "mail", Action: "send"}}); instruction != "" {
		t.Fatalf("instruction without binding tools = %q", instruction)
	}
	instruction := routine.BindingToolGrantInstruction([]skill.ModelAction{
		{Name: "mail.send", SkillID: "mail", Action: "send"},
		{Name: "mcp.github.create_issue", SkillID: "openseal.connector", Action: "call", BindingTool: &skill.ModelActionBindingTool{Name: "create_issue"}},
	})
	if !strings.Contains(instruction, "mcp.github.create_issue") || !strings.Contains(instruction, "openseal.connector.call itself cannot be granted") {
		t.Fatalf("instruction = %q", instruction)
	}
}
