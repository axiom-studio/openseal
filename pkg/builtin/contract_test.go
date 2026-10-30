package builtin

import (
	"fmt"
	"strings"
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

func TestFileToolContractsTakePathsAndRejectForeignInput(t *testing.T) {
	catalog, definition := builtinContractCatalog(t)
	sheets := []interface{}{map[string]interface{}{"name": "Budget", "rows": []interface{}{[]interface{}{"Item", 12.5, true, nil}}}}
	cases := []struct {
		name   string
		action string
		input  map[string]interface{}
		valid  bool
	}{
		{"save app", SaveApp, map[string]interface{}{"path": "apps/tracker", "title": "Tracker"}, true},
		{"save app without a path", SaveApp, map[string]interface{}{"title": "Tracker"}, false},
		{"save app with foreign field", SaveApp, map[string]interface{}{"path": "apps/tracker", "provider": "caller-chosen"}, false},
		{"publish", PublishApp, map[string]interface{}{"path": "apps/tracker"}, true},
		{"save data", SaveAppData, map[string]interface{}{"path": "apps/tracker", "name": "pipeline_by_stage", "value": []interface{}{map[string]interface{}{"stage": "Discovery", "value": 1620000}}}, true},
		{"save data with a path name", SaveAppData, map[string]interface{}{"path": "apps/tracker", "name": "../secrets", "value": 1}, false},
		{"save data without a value", SaveAppData, map[string]interface{}{"path": "apps/tracker", "name": "pipeline"}, false},
		{"present", Present, map[string]interface{}{"paths": []interface{}{"pages/report.html", "docs/report.pdf"}}, true},
		{"present nothing", Present, map[string]interface{}{"paths": []interface{}{}}, false},
		{"image", GenerateImage, map[string]interface{}{"prompt": "a seal", "path": "images/seal.png"}, true},
		{"image with a style", GenerateImage, map[string]interface{}{"prompt": "a seal", "style": "cartoon"}, true},
		{"image with an empty style", GenerateImage, map[string]interface{}{"prompt": "a seal", "style": ""}, false},
		{"image with a long style", GenerateImage, map[string]interface{}{"prompt": "a seal", "style": strings.Repeat("é", MaximumImageStyleRunes+1)}, false},
		{"set style", SetImageStyle, map[string]interface{}{"style": strings.Repeat("é", MaximumImageStyleRunes)}, true},
		{"clear style", SetImageStyle, map[string]interface{}{"style": ""}, true},
		{"set style without a style", SetImageStyle, map[string]interface{}{}, false},
		{"set style for someone else", SetImageStyle, map[string]interface{}{"style": "cartoon", "userId": "23"}, false},
		{"read data by path", ReadAppData, map[string]interface{}{"app": "apps/tracker", "name": "scores"}, true},
		{"read data by id", ReadAppData, map[string]interface{}{"app": "app-" + strings.Repeat("a", 64), "name": "scores"}, true},
		{"read data without an app", ReadAppData, map[string]interface{}{"name": "scores"}, false},
		{"read data with a path name", ReadAppData, map[string]interface{}{"app": "apps/tracker", "name": "../secrets"}, false},
		{"find a place", FindPlace, map[string]interface{}{"query": "Lalbagh Botanical Garden"}, true},
		{"find places near one", FindPlace, map[string]interface{}{"query": "cafes", "near": "osm:W4389201", "limit": 5}, true},
		{"find a place without a query", FindPlace, map[string]interface{}{"limit": 3}, false},
		{"find a place with an empty query", FindPlace, map[string]interface{}{"query": ""}, false},
		{"find a place with a long query", FindPlace, map[string]interface{}{"query": strings.Repeat("a", MaximumPlaceQueryRunes+1)}, false},
		{"find places near free text", FindPlace, map[string]interface{}{"query": "cafes", "near": "Indiranagar"}, false},
		{"find too many places", FindPlace, map[string]interface{}{"query": "cafes", "limit": 6}, false},
		{"find a place at coordinates", FindPlace, map[string]interface{}{"query": "cafes", "lat": 12.9, "lon": 77.6}, false},
		{"pdf", CreatePDF, map[string]interface{}{"path": "docs/brief.pdf", "blocks": []interface{}{map[string]interface{}{"kind": "image", "path": "images/seal.png"}}}, true},
		{"pdf with a revision id", CreatePDF, map[string]interface{}{"path": "docs/brief.pdf", "documentId": "brief"}, false},
		{"sheet", CreateSheet, map[string]interface{}{"path": "sheets/budget.xlsx", "sheets": sheets}, true},
		{"sheet without a path", CreateSheet, map[string]interface{}{"sheets": sheets}, false},
	}
	for _, tc := range cases {
		err := catalog.ValidateDefinitionInput(t.Context(), definition.ID, definition.Version, skill.DefinitionSourceIdentity(definition), tc.action, tc.input)
		if tc.valid && err != nil || !tc.valid && err == nil {
			t.Fatalf("%s: schema validation: %v", tc.name, err)
		}
	}
}
