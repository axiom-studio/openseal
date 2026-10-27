package runtime

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func configUpgradeBase() *skill.Definition {
	readAction := func(name string) skill.Action {
		return skill.Action{Name: name, Description: name, InputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}}
	}
	return &skill.Definition{
		ID: "skill-browser", Version: "1.0.0", Name: "Browser",
		Transport: skill.TransportReference{Kind: "grpc", Endpoint: "skill-browser:50051"},
		Actions:   map[string]skill.Action{"search": readAction("search"), "config-check": readAction("config-check"), "config-check-v2": readAction("config-check-v2")},
		Config: &skill.SkillConfig{
			Scopes: []skill.SkillConfigScope{"global", "tenant"},
			Fields: []skill.SkillConfigField{
				{Key: "searchProvider", Label: "Search provider", Type: "enum", Enum: []string{"yahoo", "tavily"}, Default: "yahoo"},
				{Key: "tavilyApiKey", Label: "Tavily API key", Type: "secret", RequiredWhen: map[string]interface{}{"searchProvider": "tavily"}},
				{Key: "fallbackToYahoo", Label: "Fall back to Yahoo", Type: "bool", Default: false},
			},
			Health: &skill.SkillConfigHealth{Action: "config-check"},
			Setup:  []skill.SkillSetupStep{{Title: "Get a Tavily key", Links: []skill.SkillSetupLink{{Label: "Tavily", URL: "https://tavily.com"}}}},
		},
	}
}

func TestSkillUpgradeConfigContractFindings(t *testing.T) {
	base := configUpgradeBase()
	for _, test := range []struct {
		name    string
		change  func(*skill.Definition)
		subject string
		from    string
		to      string
	}{
		{"added required without default", func(d *skill.Definition) {
			d.Config.Fields = append(d.Config.Fields, skill.SkillConfigField{Key: "region", Label: "Region", Type: "string", Required: true})
		}, "region", "", "string"},
		{"added conditionally required", func(d *skill.Definition) {
			d.Config.Fields = append(d.Config.Fields, skill.SkillConfigField{Key: "tavilyRegion", Label: "Region", Type: "string", RequiredWhen: map[string]interface{}{"searchProvider": "tavily"}})
		}, "tavilyRegion", "", "string"},
		{"removed field", func(d *skill.Definition) { d.Config.Fields = d.Config.Fields[:2] }, "fallbackToYahoo", "bool", ""},
		{"removed config", func(d *skill.Definition) { d.Config = nil }, "config.scopes", "global,tenant", ""},
		{"type", func(d *skill.Definition) { d.Config.Fields[2].Type, d.Config.Fields[2].Default = "string", "no" }, "fallbackToYahoo", "bool", "string"},
		{"enum", func(d *skill.Definition) { d.Config.Fields[0].Enum = []string{"yahoo", "tavily", "bing"} }, "searchProvider", "yahoo,tavily", "yahoo,tavily,bing"},
		{"field scopes", func(d *skill.Definition) { d.Config.Fields[1].Scopes = []skill.SkillConfigScope{"global"} }, "tavilyApiKey", "global,tenant", "global"},
		{"section scopes", func(d *skill.Definition) { d.Config.Scopes = []skill.SkillConfigScope{"global"} }, "config.scopes", "global,tenant", "global"},
		{"audience", func(d *skill.Definition) { d.Config.Fields[1].Audience = "host" }, "tavilyApiKey", "skill", "host"},
		{"became required", func(d *skill.Definition) { d.Config.Fields[2].Default, d.Config.Fields[2].Required = nil, true }, "fallbackToYahoo", "", ""},
		{"health action", func(d *skill.Definition) { d.Config.Health.Action = "config-check-v2" }, "config.health.action", "config-check", "config-check-v2"},
		{"health removed", func(d *skill.Definition) { d.Config.Health = nil }, "config.health.action", "config-check", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := cloneUpgradeTestDefinition(t, base)
			target.Version = "1.1.0"
			test.change(target)
			findings := compareUpgradeContracts(base, target, []string{"search"}, nil)
			for _, finding := range findings {
				if finding.Code == "config_contract_changed" && finding.Subject == test.subject && finding.From == test.from && finding.To == test.to && finding.Message != "" {
					return
				}
			}
			t.Fatalf("missing config_contract_changed %s %q→%q: %#v", test.subject, test.from, test.to, findings)
		})
	}
	added := cloneUpgradeTestDefinition(t, base)
	added.Config = nil
	if findings := compareUpgradeContracts(added, base, []string{"search"}, nil); len(findings) == 0 {
		t.Fatal("adding a config section with a health action needs review")
	}
}

func TestSkillUpgradeConfigCompatibleChangesNeedNoApproval(t *testing.T) {
	base := configUpgradeBase()
	for name, change := range map[string]func(*skill.Definition){
		"optional field": func(d *skill.Definition) {
			d.Config.Fields = append(d.Config.Fields, skill.SkillConfigField{Key: "safeSearch", Label: "Safe search", Type: "bool"})
		},
		"required field with default": func(d *skill.Definition) {
			d.Config.Fields = append(d.Config.Fields, skill.SkillConfigField{Key: "maxResults", Label: "Max results", Type: "int", Required: true, Default: 10})
		},
		"text and setup": func(d *skill.Definition) {
			d.Config.Fields[0].Label, d.Config.Fields[0].Description, d.Config.Fields[0].Summary = "Provider", "Which engine to use.", true
			d.Config.Setup = append(d.Config.Setup, skill.SkillSetupStep{Title: "Copy the callback", Copy: &skill.SkillSetupCopy{Platform: "oauthCallbackUrl"}})
			d.Config.Setup[0].Title = "Create a Tavily API key"
		},
		"default changed": func(d *skill.Definition) { d.Config.Fields[2].Default = true },
		"explicit default scopes": func(d *skill.Definition) {
			d.Config.Fields[0].Scopes = []skill.SkillConfigScope{"tenant", "global"}
			d.Config.Fields[1].Audience = "skill"
		},
	} {
		t.Run(name, func(t *testing.T) {
			target := cloneUpgradeTestDefinition(t, base)
			target.Version = "1.1.0"
			change(target)
			if findings := compareUpgradeContracts(base, target, []string{"search"}, nil); len(findings) != 0 {
				t.Fatalf("compatible config change requires approval: %#v", findings)
			}
		})
	}
}

func TestSkillConfigPersistsWithDefinitionAcrossStores(t *testing.T) {
	sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	for name, store := range map[string]skill.CatalogStore{"memory": NewMemoryStore(), "sqlite": sqlite} {
		t.Run(name, func(t *testing.T) {
			definition := configUpgradeBase()
			if err := skill.NewCatalogWithStore(store).Register(t.Context(), definition); err != nil {
				t.Fatal(err)
			}
			restored, err := skill.NewCatalogWithStore(store).GetDefinition(t.Context(), definition.ID, definition.Version)
			if err != nil {
				t.Fatal(err)
			}
			// JSON storage decodes numbers as float64; compare through a clone.
			if !reflect.DeepEqual(restored.Config, cloneUpgradeTestDefinition(t, definition).Config) {
				t.Fatalf("config did not round-trip:\n%#v\n%#v", restored.Config, definition.Config)
			}
		})
	}
}
