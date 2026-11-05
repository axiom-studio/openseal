package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillDefinitionRegistersAndKeepsPolicies(t *testing.T) {
	d := SkillDefinition()
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if d.ID != SkillID || d.Version != "1.2.0" || len(d.Actions) != 3 {
		t.Fatalf("definition = %s@%s with %d actions", d.ID, d.Version, len(d.Actions))
	}
	read, call, refresh := d.Actions[Read], d.Actions[Call], d.Actions[RefreshTools]
	if read.SideEffect != skill.SideEffectRead || read.Risk != skill.RiskLevelRead || refresh.SideEffect != skill.SideEffectRead {
		t.Fatal("read and refresh_tools must be read-only")
	}
	if call.SideEffect != skill.SideEffectWrite || call.Idempotency != skill.IdempotencyRequired {
		t.Fatal("call must be a write requiring idempotency")
	}
	if read.Idempotency != skill.IdempotencySupported || refresh.Idempotency != skill.IdempotencySupported {
		t.Fatal("read and refresh_tools must support idempotency like the builtin reads")
	}
	if spec := d.BindingTools; spec == nil || spec.ModelPrefix != ModelPrefix || spec.ReadAction != Read || spec.WriteAction != Call ||
		spec.InputArgument != "arguments" || !reflect.DeepEqual(spec.FixedArguments, []string{"tool", "hash"}) {
		t.Fatalf("binding tools = %#v", d.BindingTools)
	}
	for name, action := range d.Actions {
		if action.Retry.MaxAttempts != 1 || action.InputSchema["additionalProperties"] != false {
			t.Errorf("%s must have a closed input and no automatic retry", name)
		}
	}
	input := map[string]interface{}{"tool": "lookup", "hash": Digest("x"), "arguments": map[string]interface{}{"key": "a"}}
	if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, skill.DefinitionSourceIdentity(d), Call, input); err != nil {
		t.Fatal(err)
	}
	input["endpoint"] = "https://elsewhere.example"
	if err := catalog.ValidateDefinitionInput(t.Context(), d.ID, d.Version, skill.DefinitionSourceIdentity(d), Call, input); err == nil {
		t.Fatal("endpoint accepted as action input")
	}
}

func TestBindingConfigSchemaMatchesStruct(t *testing.T) {
	encoded, _ := json.Marshal(validConfig())
	var value map[string]interface{}
	_ = json.Unmarshal(encoded, &value)
	s, err := compileSchema(bindingConfigSchema())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.validate(value); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBindingConfig(value); err != nil {
		t.Fatal(err)
	}
}

func TestSkillCredentialSlotsAreSecretFields(t *testing.T) {
	d := SkillDefinition()
	for name, action := range d.Actions {
		if !reflect.DeepEqual(action.Credentials, Credentials()) {
			t.Fatalf("%s credentials = %#v", name, action.Credentials)
		}
		for _, requirement := range action.Credentials {
			if !requirement.Optional {
				t.Fatalf("%s credential %s must be optional so open servers need none", name, requirement.Name)
			}
		}
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[AuthKind][]string{
		AuthNone: nil, AuthOAuth: {SecretAccess}, AuthBearer: {SecretAccess}, AuthAPIKey: {SecretAccess}, AuthBasic: {SecretUsername, SecretPassword},
	} {
		config := BindingConfig{Auth: Auth{Kind: kind}}
		if got := config.CredentialSlots(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s slots = %v, want %v", kind, got, want)
		}
		config.SecretHeaders = []SecretHeader{{Name: "X-Workspace", Field: "workspace"}}
		if got := config.CredentialSlots(); !reflect.DeepEqual(got, append(append([]string(nil), want...), CredentialHeaders)) {
			t.Errorf("%s slots with secret headers = %v", kind, got)
		}
	}
	refresh := d.Actions[RefreshTools].OutputSchema["properties"].(map[string]interface{})
	for _, field := range []string{"tools", "bindingTools", "unsupported"} {
		if refresh[field] == nil {
			t.Errorf("refresh_tools output lacks %s", field)
		}
	}
}
