package skill

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const configExampleManifest = `apiVersion: openseal.dev/v1alpha1
kind: SkillDefinition
definition:
  id: skill-browser
  version: 3.0.2
  name: Browser
  transport: {kind: grpc, endpoint: "skill-browser:50051"}
  actions:
    search:
      name: search
      description: Search the web.
      sideEffect: read
      risk: read
      idempotency: supported
      retry: {maxAttempts: 1}
      inputSchema:
        type: object
        properties:
          query: {type: string}
    config-check:
      name: config-check
      description: Check the effective configuration.
      sideEffect: read
      risk: read
      idempotency: supported
      retry: {maxAttempts: 1}
      inputSchema: {type: object}
  config:
    scopes: [global, tenant]
    fields:
      - key: searchProvider
        label: Search provider
        description: Engine behind web search.
        type: enum
        enum: [yahoo, tavily]
        default: yahoo
        summary: true
      - key: tavilyApiKey
        label: Tavily API key
        type: secret
        requiredWhen: {searchProvider: tavily}
        summary: true
      - key: fallbackToYahoo
        label: Fall back to Yahoo
        type: bool
        default: false
        summary: true
      - key: maxResults
        label: Max results
        type: int
        default: 10
        min: 1
        max: 50
      - key: oauthClientId
        label: OAuth client ID
        type: string
        pattern: "^[0-9a-z.-]+$"
        required: true
        scopes: [global]
        audience: host
      - key: oauthClientSecret
        label: OAuth client secret
        type: secret
        required: true
        scopes: [global]
        audience: host
      - key: homePage
        label: Home page
        type: url
        default: "https://example.com/"
    setup:
      - title: Create an OAuth client
        description: Web application type, in Google Cloud Console.
        links: [{label: Google Cloud Console, url: "https://console.cloud.google.com/apis/credentials"}]
      - title: Enable the APIs
        links:
          - {label: Gmail API, url: "https://console.cloud.google.com/apis/library/gmail.googleapis.com"}
          - {label: Google Drive API, url: "https://console.cloud.google.com/apis/library/drive.googleapis.com"}
      - title: Add the redirect URI
        copy: {platform: oauthCallbackUrl}
      - title: Note the client ID
        copy: {field: oauthClientId}
`

func configExampleDefinition(t *testing.T) *Definition {
	t.Helper()
	manifest, err := DecodeManifestYAML([]byte(configExampleManifest))
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition(manifest.Definition)
	return &definition
}

func TestSkillConfigFullExampleIsValidAndRoundTrips(t *testing.T) {
	definition := configExampleDefinition(t)
	if definition.Config == nil || len(definition.Config.Fields) != 7 || len(definition.Config.Setup) != 4 {
		t.Fatalf("config was not decoded: %#v", definition.Config)
	}
	if definition.Config.Setup[2].Copy.Platform != capability.SkillSetupPlatformOAuthCallbackURL || definition.Config.Fields[4].EffectiveAudience() != capability.SkillConfigAudienceHost {
		t.Fatalf("decoded config = %#v", definition.Config)
	}
	manifest, err := NewManifest(definition)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeManifestYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifestYAML(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Definition.Config, definition.Config) {
		t.Fatalf("YAML round trip changed config:\n%#v\n%#v", decoded.Definition.Config, definition.Config)
	}
	clone := cloneDefinition(definition)
	if !reflect.DeepEqual(clone.Config, definition.Config) {
		t.Fatalf("JSON clone changed config")
	}
	payload, err := json.Marshal(definition)
	if err != nil || !strings.Contains(string(payload), `"config":{"scopes":["global","tenant"]`) || !strings.Contains(string(payload), `"setup":[`) {
		t.Fatalf("JSON payload = %s, %v", payload, err)
	}
	// Go-constructed defaults and requiredWhen values validate the same way.
	definition.Config.Fields[3].Default = 7
	if err := validateDefinition(definition); err != nil {
		t.Fatal(err)
	}
	withoutConfig := configExampleDefinition(t)
	withoutConfig.Config = nil
	digestWith, _ := DefinitionDigest(configExampleDefinition(t))
	digestWithout, _ := DefinitionDigest(withoutConfig)
	if digestWith == "" || digestWith == digestWithout {
		t.Fatal("config is not part of the definition digest")
	}
}

func TestSkillConfigValidationRejectsEachRule(t *testing.T) {
	int64p := func(value int64) *int64 { return &value }
	cases := map[string]struct {
		change func(*Definition)
		want   string
	}{
		"empty section scopes":    {func(d *Definition) { d.Config.Scopes = nil }, "scopes must name"},
		"unknown section scope":   {func(d *Definition) { d.Config.Scopes = []SkillConfigScope{"user"} }, "is not global or tenant"},
		"duplicate section scope": {func(d *Definition) { d.Config.Scopes = []SkillConfigScope{"global", "global"} }, "duplicated"},
		"no fields":               {func(d *Definition) { d.Config.Fields = nil; d.Config.Setup = nil }, "at least one field"},
		"key regex uppercase":     {func(d *Definition) { d.Config.Fields[0].Key = "SearchProvider" }, "must match"},
		"key regex dash":          {func(d *Definition) { d.Config.Fields[2].Key = "fallback-yahoo" }, "must match"},
		"key too long":            {func(d *Definition) { d.Config.Fields[2].Key = "a" + strings.Repeat("b", 64) }, "must match"},
		"duplicate key":           {func(d *Definition) { d.Config.Fields[2].Key = "searchProvider" }, "duplicated"},
		"missing label":           {func(d *Definition) { d.Config.Fields[0].Label = "" }, "label is required"},
		"long label":              {func(d *Definition) { d.Config.Fields[0].Label = strings.Repeat("x", 61) }, "at most 60"},
		"long description":        {func(d *Definition) { d.Config.Fields[0].Description = strings.Repeat("x", 201) }, "at most 200"},
		"unknown type":            {func(d *Definition) { d.Config.Fields[2].Type = "boolean" }, "must be one of"},
		"enum without values":     {func(d *Definition) { d.Config.Fields[0].Enum = nil }, "enum must list"},
		"duplicate enum value":    {func(d *Definition) { d.Config.Fields[0].Enum = []string{"yahoo", "yahoo"} }, "duplicated"},
		"enum on non-enum":        {func(d *Definition) { d.Config.Fields[2].Enum = []string{"true"} }, "only allowed for type enum"},
		"default not in enum":     {func(d *Definition) { d.Config.Fields[0].Default = "bing" }, "default is invalid"},
		"default wrong type":      {func(d *Definition) { d.Config.Fields[2].Default = "false" }, "default is invalid"},
		"default fraction int":    {func(d *Definition) { d.Config.Fields[3].Default = 2.5 }, "default is invalid"},
		"default below min":       {func(d *Definition) { d.Config.Fields[3].Default = 0 }, "default is invalid"},
		"default bad url":         {func(d *Definition) { d.Config.Fields[6].Default = "example.com" }, "default is invalid"},
		"default bad pattern":     {func(d *Definition) { d.Config.Fields[4].Default = "UPPER" }, "default is invalid"},
		"default on secret":       {func(d *Definition) { d.Config.Fields[1].Default = "key" }, "not allowed for type secret"},
		"requiredWhen unknown":    {func(d *Definition) { d.Config.Fields[1].RequiredWhen = map[string]interface{}{"provider": "tavily"} }, "undeclared field"},
		"requiredWhen self": {func(d *Definition) {
			d.Config.Fields[0].RequiredWhen = map[string]interface{}{"searchProvider": "yahoo"}
		}, "references itself"},
		"requiredWhen bad value": {func(d *Definition) {
			d.Config.Fields[1].RequiredWhen = map[string]interface{}{"searchProvider": "bing"}
		}, "requiredWhen value"},
		"requiredWhen secret":      {func(d *Definition) { d.Config.Fields[0].RequiredWhen = map[string]interface{}{"tavilyApiKey": "x"} }, "cannot compare secret"},
		"requiredWhen empty":       {func(d *Definition) { d.Config.Fields[1].RequiredWhen = map[string]interface{}{} }, "at least one field"},
		"field scope outside":      {func(d *Definition) { d.Config.Scopes = []SkillConfigScope{"tenant"}; d.Config.Fields[0].Scopes = nil }, "not one of the section's scopes"},
		"field scope duplicate":    {func(d *Definition) { d.Config.Fields[4].Scopes = []SkillConfigScope{"global", "global"} }, "duplicated"},
		"bad audience":             {func(d *Definition) { d.Config.Fields[4].Audience = "platform" }, "must be skill or host"},
		"min on string":            {func(d *Definition) { d.Config.Fields[4].Min = int64p(1) }, "only allowed for type int"},
		"max on bool":              {func(d *Definition) { d.Config.Fields[2].Max = int64p(1) }, "only allowed for type int"},
		"min above max":            {func(d *Definition) { d.Config.Fields[3].Min = int64p(60) }, "must not exceed max"},
		"pattern on enum":          {func(d *Definition) { d.Config.Fields[0].Pattern = "^y" }, "only allowed for type string"},
		"pattern does not compile": {func(d *Definition) { d.Config.Fields[4].Pattern = "([a-z" }, "does not compile"},
		"setup missing title":      {func(d *Definition) { d.Config.Setup[0].Title = "" }, "title is required"},
		"setup link no label":      {func(d *Definition) { d.Config.Setup[0].Links[0].Label = "" }, "label is required"},
		"setup link http":          {func(d *Definition) { d.Config.Setup[0].Links[0].URL = "http://console.cloud.google.com" }, "https URL"},
		"setup link relative":      {func(d *Definition) { d.Config.Setup[0].Links[0].URL = "/apis/credentials" }, "https URL"},
		"copy undeclared field":    {func(d *Definition) { d.Config.Setup[3].Copy.Field = "callbackUrl" }, "not a declared config field"},
		"copy secret field":        {func(d *Definition) { d.Config.Setup[3].Copy.Field = "oauthClientSecret" }, "is a secret"},
		"copy unknown platform":    {func(d *Definition) { d.Config.Setup[2].Copy.Platform = "tenantUrl" }, "must be one of oauthCallbackUrl"},
		"copy both":                {func(d *Definition) { d.Config.Setup[2].Copy.Field = "oauthClientId" }, "not both"},
		"copy neither":             {func(d *Definition) { d.Config.Setup[2].Copy = &SkillSetupCopy{} }, "exactly one of field or platform"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			definition := configExampleDefinition(t)
			test.change(definition)
			err := validateDefinition(definition)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSkillConfigRejectsKeysSettableElsewhere(t *testing.T) {
	cases := map[string]func(*Definition){
		"action argument": func(d *Definition) {
			d.Actions["search"].InputSchema["properties"].(map[string]interface{})["searchProvider"] = map[string]interface{}{"type": "string"}
		},
		"binding configuration key": func(d *Definition) {
			d.BindingConfigSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"fallbackToYahoo": map[string]interface{}{"type": "boolean"}}}
		},
		"action credential": func(d *Definition) {
			a := d.Actions["search"]
			a.Credentials = []CredentialRequirement{{Name: "tavilyApiKey", Kind: "api-key"}}
			d.Actions["search"] = a
		},
		"prompt credential": func(d *Definition) {
			d.Prompt = &PromptModule{Instructions: "Search.", Credentials: []CredentialRequirement{{Name: "oauthClientSecret", Kind: "oauth"}}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			definition := configExampleDefinition(t)
			change(definition)
			err := validateDefinition(definition)
			if err == nil || !strings.Contains(err.Error(), "collides with") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSkillConfigValueValidation(t *testing.T) {
	definition := configExampleDefinition(t)
	fields := map[string]SkillConfigField{}
	for _, field := range definition.Config.Fields {
		fields[field.Key] = field
	}
	for _, test := range []struct {
		key   string
		value interface{}
		ok    bool
	}{
		{"searchProvider", "tavily", true}, {"searchProvider", "bing", false},
		{"tavilyApiKey", "tvly-1", true}, {"tavilyApiKey", "", false},
		{"fallbackToYahoo", true, true}, {"fallbackToYahoo", "true", false},
		{"maxResults", float64(50), true}, {"maxResults", 51, false},
		{"homePage", "https://a.example/x", true}, {"homePage", "ftp://a.example", false},
		{"oauthClientId", "abc.apps", true}, {"oauthClientId", "ABC", false},
		{"searchProvider", nil, false},
	} {
		if err := capability.ValidateSkillConfigValue(fields[test.key], test.value); (err == nil) != test.ok {
			t.Errorf("%s=%#v: err = %v", test.key, test.value, err)
		}
	}
	if !capability.SkillConfigValuesEqual(fields["maxResults"], 3, float64(3)) || capability.SkillConfigValuesEqual(fields["searchProvider"], "a", "b") {
		t.Fatal("value equality is wrong")
	}
	if got := capability.SkillSetupPlatformValues(); !reflect.DeepEqual(got, []string{"oauthCallbackUrl"}) {
		t.Fatalf("platform values = %v", got)
	}
}
