package skill_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// mcpSnapshot is one tool as discovery decodes it from JSON.
func mcpSnapshot(name string, readOnly bool) mcp.ToolSnapshot {
	tool := map[string]interface{}{
		"name": name, "description": "The " + name + " tool",
		"annotations": map[string]interface{}{"readOnlyHint": readOnly},
		"inputSchema": map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"query": map[string]interface{}{"type": "string", "minLength": float64(1)}},
			"required":   []interface{}{"query"},
		},
		"outputSchema": map[string]interface{}{"type": "object"},
	}
	return mcp.ToolSnapshot{Tool: tool, Hash: mcp.Digest(tool)}
}

func mcpBinding(t *testing.T, id, slug string, snapshots ...mcp.ToolSnapshot) *skill.Binding {
	t.Helper()
	tools := make([]skill.BindingTool, 0, len(snapshots))
	for _, snapshot := range snapshots {
		tool, err := snapshot.BindingTool("")
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool)
	}
	return &skill.Binding{
		ID: id, Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent",
		SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion, Revision: 1,
		AllowedActions: []string{mcp.Read, mcp.Call, mcp.RefreshTools}, MaximumRisk: skill.RiskLevelWrite,
		Slug: slug, Tools: tools,
		Config: map[string]interface{}{"endpoint": "https://" + slug + ".example/mcp", "transport": "streamable-http", "auth": map[string]interface{}{"kind": "none"}},
	}
}

// Two servers bound through the real openseal.mcp definition each expose
// their pinned tools as mcp.<slug>.<tool>, compiled to {tool, hash,
// arguments} and routed to read or call by the pinned effect.
func TestMCPBindingToolsActivateAndRouteByEffect(t *testing.T) {
	ctx := context.Background()
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	search, create := mcpSnapshot("issues.search", true), mcpSnapshot("create_issue", false)
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, mcp.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []*skill.Binding{
		mcpBinding(t, "github-binding", "github", search, create),
		mcpBinding(t, "linear-binding", "linear", search),
	} {
		if err := catalog.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := catalog.Activate(ctx, scope, "agent", skill.HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]skill.ModelAction{}
	for _, activated := range snapshot.Skills {
		for _, action := range activated.Actions {
			if action.BindingTool != nil {
				actions[action.Name] = action
			}
		}
	}
	for name, want := range map[string]struct {
		binding, action string
		snapshot        mcp.ToolSnapshot
	}{
		"mcp.github.issues_search": {"github-binding", mcp.Read, search},
		"mcp.github.create_issue":  {"github-binding", mcp.Call, create},
		"mcp.linear.issues_search": {"linear-binding", mcp.Read, search},
	} {
		action, ok := actions[name]
		if !ok {
			t.Fatalf("model action %s missing from %v", name, actions)
		}
		remote, _ := want.snapshot.Tool["name"].(string)
		if action.BindingID != want.binding || action.Action != want.action || action.Description != "The "+remote+" tool" ||
			!reflect.DeepEqual(action.InputSchema, want.snapshot.Tool["inputSchema"]) {
			t.Fatalf("%s = %#v", name, action)
		}
		input := action.ActionInput(map[string]interface{}{"query": "openseal"})
		wantInput := map[string]interface{}{"tool": remote, "hash": want.snapshot.Hash, "arguments": map[string]interface{}{"query": "openseal"}}
		if !reflect.DeepEqual(input, wantInput) {
			t.Fatalf("%s compiled input = %#v", name, input)
		}
		if decoded, err := mcp.DecodeInput(input); err != nil || decoded.Tool != remote || decoded.Hash != want.snapshot.Hash {
			t.Fatalf("%s input does not decode as an MCP input: %v", name, err)
		}
		bound, err := catalog.Resolve(ctx, scope, "agent", action.SkillID, action.Version, action.Action, skill.BindingReference{ID: action.BindingID, Revision: action.BindingRevision})
		if err != nil {
			t.Fatal(err)
		}
		if err := catalog.ValidateInput(ctx, bound, input); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(actions) != 3 {
		t.Fatalf("binding tool actions = %v", actions)
	}
	// A read tool cannot be reached through call, nor a write tool through read.
	for action, tool := range map[string]mcp.ToolSnapshot{mcp.Call: search, mcp.Read: create} {
		bound, err := catalog.Resolve(ctx, scope, "agent", mcp.SkillID, mcp.SkillVersion, action, skill.BindingReference{ID: "github-binding", Revision: 1})
		if err != nil {
			t.Fatal(err)
		}
		input := map[string]interface{}{"tool": tool.Tool["name"], "hash": tool.Hash, "arguments": map[string]interface{}{"query": "x"}}
		if err := catalog.ValidateInput(ctx, bound, input); err == nil {
			t.Fatalf("%s accepted tool %v", action, tool.Tool["name"])
		}
	}
}
