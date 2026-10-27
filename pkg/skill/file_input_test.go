package skill

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/fileinput"
)

func TestFileInputsAreChatPathsToTheModelAndKernel(t *testing.T) {
	catalog := NewCatalog()
	def := testSkillDefinition()
	deploy := def.Actions["deploy"]
	deploy.InputSchema["properties"].(map[string]interface{})["notes"] = map[string]interface{}{"type": fileinput.Type, "description": "Release notes."}
	def.Actions["deploy"] = deploy
	if err := catalog.Register(t.Context(), def); err != nil {
		t.Fatalf("file input rejected: %v", err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	binding, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: &Binding{ID: "release", Scope: scope, DeploymentID: "agent", SkillID: def.ID, SkillVersion: def.Version,
		MaximumRisk: RiskLevelProduction, AllowedActions: []string{"deploy"}, Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential://test-only"}}},
		Actor: BindingActor{Type: "user", ID: "one"}, Reason: "Release"})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := catalog.ListModelActions(t.Context(), scope, "agent")
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions: %#v %v", actions, err)
	}
	notes := actions[0].InputSchema["properties"].(map[string]interface{})["notes"].(map[string]interface{})
	if notes["type"] != "string" {
		t.Fatalf("model sees file input as %#v", notes)
	}
	bound, err := catalog.Resolve(t.Context(), scope, "agent", def.ID, def.Version, "deploy", BindingReference{ID: binding.ID, Revision: binding.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateInput(t.Context(), bound, map[string]interface{}{"environment": "prod", "revision": "r1", "notes": "docs/notes.md"}); err != nil {
		t.Fatalf("chat path rejected: %v", err)
	}
	if err := catalog.ValidateInput(t.Context(), bound, map[string]interface{}{"environment": "prod", "revision": "r1", "notes": 5}); err == nil {
		t.Fatal("non-path file input accepted")
	}

	nested := testSkillDefinition()
	nested.ID = "nested"
	action := nested.Actions["deploy"]
	action.InputSchema["properties"].(map[string]interface{})["bundle"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"notes": map[string]interface{}{"type": fileinput.Type}}}
	nested.Actions["deploy"] = action
	if err := NewCatalog().Register(t.Context(), nested); err == nil {
		t.Fatal("nested file input accepted; only top-level inputs are handed over")
	}
}
