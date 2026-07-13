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
	if actions[0].BindingID != binding.ID || actions[0].BindingRevision != binding.Revision || actions[0].SemanticArguments["target"] != "environment" {
		t.Fatalf("action did not preserve its exact secret-safe binding projection: %#v", actions[0])
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

func TestCatalogResolvesAnExactBindingAndRejectsStaleSelection(t *testing.T) {
	catalog := NewCatalog()
	definition := testSkillDefinition()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	for _, id := range []string{"primary", "secondary"} {
		if err := catalog.Bind(context.Background(), &Binding{
			ID: id, Scope: scope, DeploymentID: "operator", SkillID: definition.ID, SkillVersion: definition.Version,
			AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction,
			Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: id + "-credential"}}, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	actions, err := catalog.ListModelActions(context.Background(), scope, "operator")
	if err != nil || len(actions) != 2 {
		t.Fatalf("actions = %#v, %v", actions, err)
	}
	selected, err := catalog.Resolve(context.Background(), scope, "operator", definition.ID, definition.Version, "deploy", BindingReference{ID: "secondary", Revision: 1})
	if err != nil || selected.Binding.ID != "secondary" {
		t.Fatalf("selected = %#v, %v", selected, err)
	}
	if _, err := catalog.Resolve(context.Background(), scope, "operator", definition.ID, definition.Version, "deploy", BindingReference{ID: "secondary", Revision: 2}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale selection error = %v", err)
	}
}

func TestCatalogDisabledBindingHidesCapabilities(t *testing.T) {
	catalog := NewCatalog()
	definition := testSkillDefinition()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	binding := &Binding{
		ID: "binding", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "operator",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"deploy"},
		MaximumRisk: RiskLevelProduction, Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "opaque"}}, Revision: 1,
	}
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	binding.Disabled = true
	binding.Revision = 2
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	actions, err := catalog.ListModelActions(context.Background(), binding.Scope, binding.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("disabled binding exposed actions: %#v", actions)
	}
	resolved, err := catalog.Resolve(context.Background(), binding.Scope, binding.DeploymentID, binding.SkillID, binding.SkillVersion, "deploy")
	if err == nil || resolved != nil {
		t.Fatalf("disabled binding resolved action: %#v, %v", resolved, err)
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

func TestMaterializeTransportArgumentsKeepsCredentialsOutOfEnvelope(t *testing.T) {
	bound := &BoundAction{
		Definition: &Definition{Transport: TransportReference{
			Kind: "tool", Endpoint: "publish",
			Arguments: map[string]TransportArgument{
				"command":     {SourceArgument: "request"},
				"commandName": {Literal: "publisher"},
			},
		}},
		Action: Action{Name: "invoke"},
	}
	arguments, err := MaterializeTransportArguments(bound, map[string]interface{}{"request": "release 1.2.3", "credential": "must-not-pass"})
	if err != nil {
		t.Fatal(err)
	}
	if arguments["command"] != "release 1.2.3" || arguments["commandName"] != "publisher" || len(arguments) != 2 {
		t.Fatalf("transport projection = %#v", arguments)
	}
	if _, err := MaterializeTransportArguments(bound, map[string]interface{}{}); err == nil {
		t.Fatal("missing mapped input should fail")
	}
}

func TestDefinitionAllowsDistinctPerActionTransports(t *testing.T) {
	definition := &Definition{ID: "builtin", Version: "1", Name: "Builtin", Actions: map[string]Action{
		"fetch":    {Name: "fetch", Description: "Fetch URL", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}, Risk: RiskLevelRead, SideEffect: SideEffectRead, Idempotency: IdempotencySupported, Transport: &TransportReference{Kind: "tool", Endpoint: "fetch-url"}},
		"download": {Name: "download", Description: "Download file", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}, Risk: RiskLevelRead, SideEffect: SideEffectRead, Idempotency: IdempotencySupported, Transport: &TransportReference{Kind: "tool", Endpoint: "download-file"}},
	}}
	if err := validateDefinition(definition); err != nil {
		t.Fatal(err)
	}
	definition.Actions["download"] = Action{Name: "download", Description: "Download file", InputSchema: map[string]interface{}{"type": "object"}, Risk: RiskLevelRead, SideEffect: SideEffectRead, Idempotency: IdempotencySupported}
	if err := validateDefinition(definition); err == nil || !strings.Contains(err.Error(), "requires an action or definition transport") {
		t.Fatalf("error = %v", err)
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
				Credentials:       []CredentialRequirement{{Name: "git", Kind: "git-token"}},
				SemanticArguments: map[string]string{"target": "environment", "artifact": "revision"},
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
