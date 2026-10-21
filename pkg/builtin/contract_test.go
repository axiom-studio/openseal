package builtin

import (
	"fmt"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func builtinContractCatalog(t *testing.T) (*skill.Catalog, *skill.Definition) {
	t.Helper()
	catalog := skill.NewCatalog()
	definition := SkillDefinition()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	return catalog, definition
}

func TestBuiltinContractVersionAdvancesPastPublishedVersions(t *testing.T) {
	var major, minor, patch int
	if n, err := fmt.Sscanf(SkillVersion, "%d.%d.%d", &major, &minor, &patch); err != nil || n != 3 {
		t.Fatalf("invalid built-in version: %q", SkillVersion)
	}
	if major < 1 || major == 1 && (minor < 5 || minor == 5 && patch <= 1) {
		t.Fatalf("new immutable contract must advance past published 1.5.1: %q", SkillVersion)
	}
}

func TestHistoryContractValidatesForwardCursorAndRejectsForeignFields(t *testing.T) {
	catalog, definition := builtinContractCatalog(t)
	for _, input := range []map[string]interface{}{{"afterSequence": 0, "limit": 2}, {"afterSequence": 17}, {"beforeSequence": 17}} {
		if err := catalog.ValidateDefinitionInput(t.Context(), definition.ID, definition.Version, skill.DefinitionSourceIdentity(definition), ReadConversationHistory, input); err != nil {
			t.Fatalf("valid history page rejected: %#v, %v", input, err)
		}
	}
	for _, input := range []map[string]interface{}{{"afterSequence": -1}, {"afterSequence": 0, "limit": 11}, {"afterSequence": 0, "conversationId": "another-chat"}} {
		if err := catalog.ValidateDefinitionInput(t.Context(), definition.ID, definition.Version, skill.DefinitionSourceIdentity(definition), ReadConversationHistory, input); err == nil {
			t.Fatalf("invalid history page accepted: %#v", input)
		}
	}
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{}, Action: definition.Actions[ReadConversationHistory]}
	if err := catalog.ValidateOutput(t.Context(), bound, map[string]interface{}{"messages": []interface{}{}, "nextAfterSequence": 17}); err != nil {
		t.Fatalf("forward cursor rejected: %v", err)
	}
	for _, output := range []map[string]interface{}{{"messages": []interface{}{}, "nextAfterSequence": 0}, {"messages": []interface{}{}, "conversationId": "another-chat"}} {
		if err := catalog.ValidateOutput(t.Context(), bound, output); err == nil {
			t.Fatalf("invalid history result accepted: %#v", output)
		}
	}
}

func TestAppContractsAcceptProjectsAndRejectForeignInput(t *testing.T) {
	catalog, definition := builtinContractCatalog(t)
	appID := "app-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	entry := map[string]interface{}{"path": "src/App.tsx", "content": "export default function App() { return <p>Seal</p>; }"}
	cases := []struct {
		name   string
		action string
		input  map[string]interface{}
		valid  bool
	}{
		{"create", CreateApp, map[string]interface{}{"title": "Seal", "files": []interface{}{entry}}, true},
		{"create without files", CreateApp, map[string]interface{}{"title": "Seal", "files": []interface{}{}}, false},
		{"create with foreign field", CreateApp, map[string]interface{}{"title": "Seal", "files": []interface{}{entry}, "provider": "caller-chosen"}, false},
		{"update", UpdateApp, map[string]interface{}{"appId": appID, "expectedLatestVersion": 1, "replace": []interface{}{map[string]interface{}{"path": "src/App.tsx", "find": "Seal", "replace": "Otter"}}, "delete": []interface{}{"src/old.ts"}}, true},
		{"update with site id", UpdateApp, map[string]interface{}{"appId": "site-0123", "expectedLatestVersion": 1}, false},
		{"read", ReadApp, map[string]interface{}{"appId": appID, "paths": []interface{}{"src/App.tsx"}}, true},
		{"publish", PublishApp, map[string]interface{}{"appId": appID, "expectedLatestVersion": 2}, true},
		{"publish without version", PublishApp, map[string]interface{}{"appId": appID}, false},
		{"save data", SaveAppData, map[string]interface{}{"appId": appID, "name": "pipeline_by_stage", "value": []interface{}{map[string]interface{}{"stage": "Discovery", "value": 1620000}}}, true},
		{"save data with a path name", SaveAppData, map[string]interface{}{"appId": appID, "name": "../secrets", "value": 1}, false},
		{"save data without a value", SaveAppData, map[string]interface{}{"appId": appID, "name": "pipeline"}, false},
	}
	for _, tc := range cases {
		err := catalog.ValidateDefinitionInput(t.Context(), definition.ID, definition.Version, skill.DefinitionSourceIdentity(definition), tc.action, tc.input)
		if tc.valid && err != nil || !tc.valid && err == nil {
			t.Fatalf("%s: schema validation: %v", tc.name, err)
		}
	}
}

func TestPageAndSheetContractsAreBuiltinAndRequireRevision(t *testing.T) {
	catalog, definition := builtinContractCatalog(t)
	if len(definition.Installers) != 0 {
		t.Fatal("builtin creation must not install a service")
	}
	for _, action := range []string{CreatePage, CreateSheet} {
		input := map[string]interface{}{"artifactId": "brief", "expectedLatestVersion": 0, "title": "Brief", "requirementName": "brief"}
		if action == CreatePage {
			input["filename"] = "brief.html"
			input["html"] = "<html><body>Brief</body></html>"
		} else {
			input["filename"] = "brief.xlsx"
			input["sheets"] = []interface{}{map[string]interface{}{"name": "Budget", "rows": []interface{}{[]interface{}{"Item", 12.5, true, nil}}}}
		}
		if err := catalog.ValidateDefinitionInput(t.Context(), definition.ID, definition.Version, skill.DefinitionSourceIdentity(definition), action, input); err != nil {
			t.Fatal(err)
		}
		delete(input, "expectedLatestVersion")
		if err := catalog.ValidateDefinitionInput(t.Context(), definition.ID, definition.Version, skill.DefinitionSourceIdentity(definition), action, input); err == nil {
			t.Fatal("missing revision accepted")
		}
	}
}
