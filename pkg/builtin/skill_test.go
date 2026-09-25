package builtin

import "testing"

func TestBuiltinAttachmentReadIsReferenceOnly(t *testing.T) {
	d := SkillDefinition()
	a := d.Actions[ReadAttachment]
	if d.ID != SkillID || len(d.Actions) != 9 || string(a.Risk) != "read" || string(a.SideEffect) != "read" {
		t.Fatal("built-in attachment action must grant read-only access")
	}
	props := a.InputSchema["properties"].(map[string]interface{})
	if len(props) != 2 || props["artifactId"] == nil || props["version"] == nil || a.InputSchema["additionalProperties"] != false {
		t.Fatal("attachment input must contain only an exact artifact version, never paths or URLs")
	}
	if !d.Requirements.AlwaysAvailable || len(d.Prompt.AllowedTools) != len(d.Actions) || d.Prompt.AllowedTools[0] != ReadAttachment {
		t.Fatal("default attachment capability prompt missing")
	}
}

func TestBuiltinCreativeActionsStayBounded(t *testing.T) {
	d := SkillDefinition()
	for _, name := range []string{GenerateImage, CreatePDF, CreateSlides, CreateSite, UpdateSite, PublishSite} {
		a, ok := d.Actions[name]
		if !ok || a.InputSchema["additionalProperties"] != false || a.Retry.MaxAttempts != 1 || string(a.Idempotency) != "required" {
			t.Fatalf("%s must have a closed input and no automatic retry", name)
		}
	}
	if string(d.Actions[PublishSite].SideEffect) != "external" || string(d.Actions[GenerateImage].SideEffect) != "write" {
		t.Fatal("publication and image generation must retain distinct side effects")
	}
	pdf := d.Actions[CreatePDF]
	properties := pdf.InputSchema["properties"].(map[string]interface{})
	for _, name := range []string{"pages", "blocks", "expectedPages", "revisionMode", "expectedLatestVersion"} {
		if properties[name] == nil {
			t.Fatalf("PDF input missing %s", name)
		}
	}
}

func TestProfileImageActionIsBoundedSelfOnlyWrite(t *testing.T) {
	a := SkillDefinition().Actions[GenerateProfileImage]
	properties := a.InputSchema["properties"].(map[string]interface{})
	if len(properties) != 1 || properties["prompt"] == nil || a.InputSchema["additionalProperties"] != false || string(a.Risk) != "write" || string(a.SideEffect) != "write" || a.Retry.MaxAttempts != 1 {
		t.Fatal("profile action must not accept target, credentials, URLs or automatic retries")
	}
}
