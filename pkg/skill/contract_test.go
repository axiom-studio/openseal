package skill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogProducesModelSafeScopedActions(t *testing.T) {
	catalog := NewCatalog()
	definition := testSkillDefinition()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.GetDefinition(context.Background(), definition.ID, definition.Version)
	if err != nil || loaded == definition || loaded.ID != definition.ID {
		t.Fatalf("immutable definition lookup = %#v, %v", loaded, err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	binding := &Binding{
		ID: "binding", Scope: scope, DeploymentID: "operator", SkillID: definition.ID,
		SkillVersion: definition.Version, AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction,
		ArgumentRestrictions: map[string]map[string]ArgumentRule{
			"deploy": {"environment": {Enum: []interface{}{"staging"}}},
		},
		Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "secret-credential-id"}}, Revision: 1,
	}
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	actions, err := catalog.ListModelActions(context.Background(), scope, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Name != "release.deploy" {
		t.Fatalf("unexpected model actions: %#v", actions)
	}
	encoded, err := json.Marshal(actions)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-credential-id") || strings.Contains(string(encoded), "credentials") || strings.Contains(string(encoded), "endpoint") {
		t.Fatalf("model catalog leaked runtime binding data: %s", encoded)
	}
	other, err := catalog.ListModelActions(context.Background(), ScopeReference{Kind: "tenant", ID: "two"}, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-scope actions leaked: %#v", other)
	}
}

func TestCatalogValidatesSchemaRestrictionsRiskAndCredentials(t *testing.T) {
	catalog := NewCatalog()
	definition := testSkillDefinition()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "local", ID: "test"}
	missingCredential := &Binding{
		ID: "missing", Scope: scope, DeploymentID: "agent", SkillID: definition.ID,
		SkillVersion: definition.Version, AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction, Revision: 1,
	}
	if err := catalog.Bind(context.Background(), missingCredential); err == nil {
		t.Fatal("binding without a required credential unexpectedly succeeded")
	}
	tooRisky := cloneBinding(missingCredential)
	tooRisky.ID = "risky"
	tooRisky.MaximumRisk = RiskLevelWrite
	tooRisky.Credentials = map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential"}}
	if err := catalog.Bind(context.Background(), tooRisky); err == nil {
		t.Fatal("binding above its maximum risk unexpectedly succeeded")
	}
	binding := cloneBinding(missingCredential)
	binding.ID = "valid"
	binding.Credentials = map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential"}}
	binding.ArgumentRestrictions = map[string]map[string]ArgumentRule{
		"deploy": {"environment": {Const: "staging"}},
	}
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(context.Background(), scope, "agent", "release", "1.0.0", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateInput(context.Background(), bound, map[string]interface{}{"environment": "staging", "revision": "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateInput(context.Background(), bound, map[string]interface{}{"environment": "production", "revision": "abc"}); err == nil {
		t.Fatal("binding restriction did not reject production")
	}
	if err := catalog.ValidateInput(context.Background(), bound, map[string]interface{}{"environment": "staging"}); err == nil {
		t.Fatal("schema did not reject a missing required field")
	}
	if _, err := catalog.Resolve(context.Background(), ScopeReference{Kind: "local", ID: "other"}, "agent", "release", "1.0.0", "deploy"); err == nil {
		t.Fatal("cross-scope action resolution unexpectedly succeeded")
	}
}

func TestCatalogRejectsInvalidJSONSchema(t *testing.T) {
	definition := testSkillDefinition()
	action := definition.Actions["deploy"]
	action.InputSchema = map[string]interface{}{"type": "not-a-json-schema-type"}
	definition.Actions["deploy"] = action
	if err := NewCatalog().Register(context.Background(), definition); err == nil {
		t.Fatal("invalid JSON Schema unexpectedly compiled")
	}
}

func TestCatalogRejectsExternalSchemaReferencesAndMutableVersions(t *testing.T) {
	definition := testSkillDefinition()
	action := definition.Actions["deploy"]
	action.InputSchema = map[string]interface{}{"$ref": "https://example.invalid/schema.json"}
	definition.Actions["deploy"] = action
	if err := NewCatalog().Register(context.Background(), definition); err == nil {
		t.Fatal("external JSON Schema reference unexpectedly compiled")
	}
	catalog := NewCatalog()
	definition = testSkillDefinition()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(context.Background(), definition); err == nil {
		t.Fatal("immutable skill version was overwritten")
	}
}

func testSkillDefinition() *Definition {
	return &Definition{
		ID: "release", Version: "1.0.0", Name: "Release", Description: "Release software",
		Transport: TransportReference{Kind: "local", Endpoint: "internal://release"},
		Actions: map[string]Action{
			"deploy": {
				Name: "deploy", Description: "Deploy a revision", SideEffect: SideEffectExternal,
				Risk: RiskLevelProduction, Idempotency: IdempotencyRequired,
				Credentials: []CredentialRequirement{{Name: "git", Kind: "git-token"}},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"environment": map[string]interface{}{"type": "string"},
						"revision":    map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"environment", "revision"},
				},
			},
		},
	}
}
