package builtin

import (
	"strings"
	"testing"
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
	if d.Version != "2.1.0" {
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
