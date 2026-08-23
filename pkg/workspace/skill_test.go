package workspace

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestWorkspaceSkillDefinitionIsPortableAndBound(t *testing.T) {
	definition := SkillDefinition()
	if err := skill.NewCatalog().Register(context.Background(), definition); err != nil {
		t.Fatalf("validate Workspace Skill: %v", err)
	}
	if definition.BindingConfigSchema == nil || len(definition.Actions) != 6 || definition.Prompt == nil {
		t.Fatalf("Workspace Skill = %#v", definition)
	}
	for name, action := range definition.Actions {
		if action.Transport == nil || action.Transport.Endpoint == "" || len(action.Permissions) != 1 {
			t.Fatalf("action %s = %#v", name, action)
		}
	}
}
