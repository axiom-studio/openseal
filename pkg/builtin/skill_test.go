package builtin

import "testing"

func TestAttachmentSkillIsReadOnlyAndReferenceOnly(t *testing.T) {
	d := SkillDefinition()
	a := d.Actions[ReadAttachment]
	if d.ID != SkillID || len(d.Actions) != 2 || string(a.Risk) != "read" || string(a.SideEffect) != "read" {
		t.Fatal("attachment skill must grant read-only access")
	}
	props := a.InputSchema["properties"].(map[string]interface{})
	if len(props) != 2 || props["artifactId"] == nil || props["version"] == nil || a.InputSchema["additionalProperties"] != false {
		t.Fatal("attachment input must contain only an exact artifact version, never paths or URLs")
	}
	if !d.Requirements.AlwaysAvailable || len(d.Prompt.AllowedTools) != 2 || d.Prompt.AllowedTools[0] != ReadAttachment {
		t.Fatal("default attachment capability prompt missing")
	}
}

func TestProfileImageActionIsBoundedSelfOnlyWrite(t *testing.T) {
	a := SkillDefinition().Actions[GenerateProfileImage]
	properties := a.InputSchema["properties"].(map[string]interface{})
	if len(properties) != 1 || properties["prompt"] == nil || a.InputSchema["additionalProperties"] != false || string(a.Risk) != "write" || string(a.SideEffect) != "write" || a.Retry.MaxAttempts != 1 {
		t.Fatal("profile action must not accept target, credentials, URLs or automatic retries")
	}
}
