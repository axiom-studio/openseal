package server

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/authoring"
)

func TestDesktopBindingConfigurationFieldsUseSchemaChoices(t *testing.T) {
	changeSet := &authoring.ChangeSet{Catalog: authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"research": {
			ID: "research", Version: "1.2.3", SourceIdentity: "https://clawhub.ai::@acme/research",
			BindingConfigSchema: map[string]interface{}{
				"type": "object", "required": []interface{}{"region"},
				"properties": map[string]interface{}{
					"region":   map[string]interface{}{"type": "string", "title": "Region", "enum": []interface{}{"us", "eu"}},
					"apiToken": map[string]interface{}{"type": "string", "enum": []interface{}{"secret"}},
				},
			},
		},
	}}}
	fields := desktopBindingConfigurationFields(changeSet)
	if len(fields) != 1 || fields[0].Key != "region" || fields[0].Prompt != "Region" || len(fields[0].Options) != 2 {
		t.Fatalf("desktop setting choices = %#v", fields)
	}
}
