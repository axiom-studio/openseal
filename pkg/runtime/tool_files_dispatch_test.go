package runtime

import (
	"context"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/toolfile"
)

// The host learns where each {"$file": path} argument goes and the action's
// output schema, so it can put files in and take them out.
func TestToolActionDispatcherCarriesFileReferences(t *testing.T) {
	definition := &skill.Definition{
		ID: "remote", Version: "1", Name: "Remote", Transport: skill.TransportReference{Kind: "tool", Endpoint: "remote"},
		BindingTools: &capability.BindingToolsSpec{ModelPrefix: "remote", ReadAction: "read", InputArgument: "arguments", FixedArguments: []string{"tool"}},
		Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read",
			InputSchema:  map[string]interface{}{"type": "object", "required": []interface{}{"tool", "arguments"}, "properties": map[string]interface{}{"tool": map[string]interface{}{"type": "string"}, "arguments": map[string]interface{}{"type": "object"}}},
			OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"content": map[string]interface{}{"type": "array"}}},
			Risk:         skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}},
	}
	bound := &skill.BoundAction{Definition: definition, Action: definition.Actions["read"], Binding: &skill.Binding{
		ID: "b", Scope: skill.ScopeReference{Kind: "test", ID: "one"}, DeploymentID: "agent",
		Tools: []skill.BindingTool{{Name: "describe", Effect: capability.BindingToolEffectRead, Arguments: map[string]interface{}{"tool": "describe"},
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"image": map[string]interface{}{"type": "string", "format": "data-url"}}}}},
	}}
	var invocation ToolInvocation
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(_ context.Context, value ToolInvocation) (map[string]interface{}, error) {
		invocation = value
		return map[string]interface{}{"content": []interface{}{}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]interface{}{"tool": "describe", "arguments": map[string]interface{}{"image": toolfile.FileValue("/uploads/cat.jpg")}}
	if _, err := dispatcher.DispatchAction(context.Background(), agentrun.ActionDispatchInput{Call: &ActionCall{ID: "call", RunID: "run"}, Bound: bound, Arguments: arguments}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(invocation.FileReferences, []toolfile.Reference{{Pointer: "/arguments/image", Path: "uploads/cat.jpg", Encoding: toolfile.DataURL}}) ||
		!reflect.DeepEqual(invocation.OutputSchema, definition.Actions["read"].OutputSchema) {
		t.Fatalf("invocation = %#v", invocation)
	}
	if _, ok := toolfile.RefPath(invocation.Arguments["arguments"].(map[string]interface{})["image"]); !ok {
		t.Fatal("the host must receive the reference, not a placeholder")
	}
	arguments["arguments"] = map[string]interface{}{"caption": toolfile.FileValue("a.png")}
	if _, err := dispatcher.DispatchAction(context.Background(), agentrun.ActionDispatchInput{Call: &ActionCall{ID: "call2", RunID: "run"}, Bound: bound, Arguments: arguments}); err == nil {
		t.Fatal("a file in a field that takes none was dispatched")
	}
}
