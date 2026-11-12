package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillManagementDispatchAcceptsCompatiblePreviousInterface(t *testing.T) {
	definition := SkillManagementSkill()
	definition.Version = "1.4.3"
	bound := &skill.BoundAction{Definition: definition, Action: definition.Actions[SkillActionRequestSetup]}
	if !isSkillBindingAction(bound) {
		t.Fatal("compatible older built-in interface rejected")
	}
	action := definition.Actions[SkillActionRequestSetup]
	action.InputSchema = map[string]interface{}{"type": "string"}
	definition.Actions[SkillActionRequestSetup] = action
	if isSkillBindingAction(bound) {
		t.Fatal("incompatible input interface accepted")
	}
	definition = SkillManagementSkill()
	definition.Source = &capability.SourceProvenance{Identity: "foreign"}
	bound.Definition = definition
	if isSkillBindingAction(bound) {
		t.Fatal("foreign source accepted as built-in")
	}
}
