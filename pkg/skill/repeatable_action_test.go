package skill

import (
	"strings"
	"testing"
)

func TestSkillManifestRepeatableRoundTripsAndValidates(t *testing.T) {
	definition := testSkillDefinition()
	action := definition.Actions["deploy"]
	action.SideEffect, action.Risk, action.ExternalOperationPolicy, action.Repeatable = SideEffectWrite, RiskLevelWrite, "", true
	definition.Actions["deploy"] = action
	manifest, err := NewManifest(definition)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeManifestYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "repeatable: true") {
		t.Fatalf("manifest YAML omits repeatable:\n%s", data)
	}
	decoded, err := DecodeManifestYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Definition.Actions["deploy"].Repeatable {
		t.Fatalf("round trip lost repeatable: %#v", decoded.Definition.Actions["deploy"])
	}
	if strings.Contains(mustEncodeManifest(t, testSkillDefinition()), "repeatable") {
		t.Fatal("a strict action writes repeatable into its manifest")
	}

	for name, mutate := range map[string]func(*Action){
		"none":        func(a *Action) { a.SideEffect, a.Risk, a.ExternalOperationPolicy = SideEffectNone, RiskLevelRead, "" },
		"read":        func(a *Action) { a.SideEffect, a.Risk, a.ExternalOperationPolicy = SideEffectRead, RiskLevelRead, "" },
		"external":    func(a *Action) { a.SideEffect = SideEffectExternal },
		"destructive": func(a *Action) { a.SideEffect, a.ExternalOperationPolicy = SideEffectDestructive, "" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := testSkillDefinition()
			action := invalid.Actions["deploy"]
			mutate(&action)
			action.Repeatable = true
			invalid.Actions["deploy"] = action
			if err := validateDefinition(invalid); err == nil || !strings.Contains(err.Error(), "repeatable") {
				t.Fatalf("repeatable %s action error = %v", name, err)
			}
		})
	}
}

func mustEncodeManifest(t *testing.T, definition *Definition) string {
	t.Helper()
	manifest, err := NewManifest(definition)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeManifestYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRepeatableReachesBindingModelActions(t *testing.T) {
	definition := bindingToolsDefinition()
	call := definition.Actions["call"]
	call.Repeatable = true
	definition.Actions["call"] = call
	if err := validateDefinition(definition); err != nil {
		t.Fatal(err)
	}
	binding := bindingToolsBinding("binding", "work", bindingToolsTool("search", BindingToolEffectRead), bindingToolsTool("post", BindingToolEffectWrite))
	binding.Tools = nil
	plain := *definition
	plain.BindingTools = nil
	direct := bindingModelActions(&plain, binding, nil)
	byAction := map[string]ModelAction{}
	for _, action := range direct {
		byAction[action.Action] = action
	}
	if !byAction["call"].Repeatable || byAction["read"].Repeatable || byAction["refresh"].Repeatable {
		t.Fatalf("definition actions lost the repeatable flag: %#v", byAction)
	}

	binding = bindingToolsBinding("binding", "work", bindingToolsTool("search", BindingToolEffectRead), bindingToolsTool("post", BindingToolEffectWrite))
	routed := map[string]ModelAction{}
	for _, action := range bindingModelActions(definition, binding, nil) {
		if action.BindingTool != nil {
			routed[action.BindingTool.Name] = action
		}
	}
	if len(routed) != 2 || !routed["post"].Repeatable || routed["search"].Repeatable {
		t.Fatalf("binding tools lost the base action's repeatable flag: %#v", routed)
	}
}
