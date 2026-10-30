package builtin

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestBuiltinAttachmentReadIsReferenceOnly(t *testing.T) {
	d := SkillDefinition()
	a := d.Actions[ReadAttachment]
	if d.ID != SkillID || string(a.Risk) != "read" || string(a.SideEffect) != "read" {
		t.Fatal("built-in attachment action must grant read-only access")
	}
	props := a.InputSchema["properties"].(map[string]interface{})
	if len(props) != 2 || props["artifactId"] == nil || props["version"] == nil || a.InputSchema["additionalProperties"] != false {
		t.Fatal("attachment input must contain only an exact artifact version, never paths or URLs")
	}
	if !d.Requirements.AlwaysAvailable || len(d.Prompt.AllowedTools) != len(d.Actions) {
		t.Fatal("every built-in tool must be in the prompt")
	}
}

func TestBuiltinMakingActionsStayBounded(t *testing.T) {
	d := SkillDefinition()
	for _, name := range []string{GenerateImage, Present, CreatePDF, CreateSheet, SaveApp, PublishApp, SaveAppData} {
		a, ok := d.Actions[name]
		if !ok || a.InputSchema["additionalProperties"] != false || a.Retry.MaxAttempts != 1 || string(a.Idempotency) != "required" {
			t.Fatalf("%s must have a closed input and no automatic retry", name)
		}
	}
	if string(d.Actions[PublishApp].SideEffect) != "external" || string(d.Actions[GenerateImage].SideEffect) != "write" {
		t.Fatal("publication and image generation must retain distinct side effects")
	}
}

func TestProfileImageActionIsBoundedSelfOnlyWrite(t *testing.T) {
	a := SkillDefinition().Actions[GenerateProfileImage]
	properties := a.InputSchema["properties"].(map[string]interface{})
	if len(properties) != 1 || properties["prompt"] == nil || a.InputSchema["additionalProperties"] != false || string(a.Risk) != "write" || string(a.SideEffect) != "write" || a.Retry.MaxAttempts != 1 {
		t.Fatal("profile action must not accept target, credentials, URLs or automatic retries")
	}
}

func TestAppDataReadAndPictureStyleActions(t *testing.T) {
	d := SkillDefinition()
	if d.Version != "2.2.0" {
		t.Fatalf("version = %s", d.Version)
	}
	read := d.Actions[ReadAppData]
	if read.Description != "Read a dataset you saved for one of your apps in this chat." || string(read.Risk) != "read" || string(read.SideEffect) != "read" {
		t.Fatalf("read_app_data = %#v", read)
	}
	style := d.Actions[SetImageStyle]
	if !strings.HasPrefix(style.Description, "Remember the picture style this user asked for") || style.InputSchema["additionalProperties"] != false || style.Retry.MaxAttempts != 1 {
		t.Fatalf("set_image_style = %#v", style)
	}
	if properties := style.InputSchema["properties"].(map[string]interface{}); len(properties) != 1 || properties["style"] == nil {
		t.Fatal("set_image_style must take only the style; the host decides whose it is")
	}
	if image := d.Actions[GenerateImage].InputSchema["properties"].(map[string]interface{})["style"].(map[string]interface{}); image["maxLength"] != MaximumImageStyleRunes {
		t.Fatalf("generate_image style = %#v", image)
	}
}

func TestFindPlaceIsReadOnlyAndReturnsPlaceIDs(t *testing.T) {
	catalog, d := builtinContractCatalog(t)
	a := d.Actions[FindPlace]
	if a.Description != "Look up real places (venues, landmarks, streets, towns) to show on a map or a place card. Refer to a result by its place id; never type coordinates." ||
		string(a.Risk) != "read" || string(a.SideEffect) != "read" || a.InputSchema["additionalProperties"] != false || a.Retry.MaxAttempts != 1 {
		t.Fatalf("find_place = %#v", a)
	}
	if properties := a.InputSchema["properties"].(map[string]interface{}); len(properties) != 3 {
		t.Fatalf("find_place input = %v", properties)
	}
	bound := &skill.BoundAction{Definition: d, Binding: &skill.Binding{}, Action: a}
	found := map[string]interface{}{"places": []interface{}{map[string]interface{}{
		"place": "place:4b2f9c0e1a77", "name": "Lalbagh Botanical Garden", "category": "garden", "address": "Mavalli, Bengaluru, Karnataka, India", "lat": 12.9507, "lon": 77.5848,
	}}}
	for _, output := range []map[string]interface{}{found, {"places": []interface{}{}}} {
		if err := catalog.ValidateOutput(t.Context(), bound, output); err != nil {
			t.Fatalf("valid find_place result rejected: %v", err)
		}
	}
	for _, output := range []map[string]interface{}{
		{"places": []interface{}{map[string]interface{}{"place": "123", "name": "x", "lat": 1, "lon": 1}}},
		{"places": []interface{}{map[string]interface{}{"place": "place:000000000001", "name": "x", "lat": 91, "lon": 1}}},
		{"places": []interface{}{map[string]interface{}{"place": "place:000000000001", "name": "x"}}},
	} {
		if err := catalog.ValidateOutput(t.Context(), bound, output); err == nil {
			t.Fatalf("invalid find_place result accepted: %v", output)
		}
	}
}

func TestBuiltinConfigHoldsTheGeoapifyKeyForTheHost(t *testing.T) {
	_, d := builtinContractCatalog(t)
	if d.Name != "Built-in tools" {
		t.Fatalf("name = %q", d.Name)
	}
	config := d.Config
	if config == nil || len(config.Scopes) != 1 || config.Scopes[0] != capability.SkillConfigScopeGlobal || len(config.Fields) != 1 || len(config.Setup) != 1 {
		t.Fatalf("config = %#v", config)
	}
	field := config.Fields[0]
	if field.Key != GeoapifyAPIKey || field.Label != "Geoapify API key" || field.Type != capability.SkillConfigFieldSecret ||
		field.EffectiveAudience() != capability.SkillConfigAudienceHost || !field.Summary || field.Required {
		t.Fatalf("field = %#v", field)
	}
	if links := config.Setup[0].Links; len(links) != 1 || links[0].URL != "https://myprojects.geoapify.com/" {
		t.Fatalf("setup = %#v", config.Setup)
	}
	for _, text := range []string{d.Name, d.Description, field.Label, field.Description, config.Setup[0].Title, config.Setup[0].Description, config.Setup[0].Links[0].Label} {
		for _, word := range []string{"openseal", "surface", "presentation", "artifact", "publish", "runbook", "objective"} {
			if strings.Contains(strings.ToLower(text), word) {
				t.Fatalf("operator text %q says %q", text, word)
			}
		}
	}
}
