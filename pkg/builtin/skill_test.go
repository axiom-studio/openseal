package builtin

import "testing"

func TestAttachmentSkillIsReadOnlyAndReferenceOnly(t *testing.T) {
	d := SkillDefinition()
	a := d.Actions[ReadAttachment]
	if d.ID != SkillID || len(d.Actions) != 1 || string(a.Risk) != "read" || string(a.SideEffect) != "read" {
		t.Fatal("attachment skill must grant read-only access")
	}
	props := a.InputSchema["properties"].(map[string]interface{})
	if len(props) != 2 || props["artifactId"] == nil || props["version"] == nil || a.InputSchema["additionalProperties"] != false {
		t.Fatal("attachment input must contain only an exact artifact version, never paths or URLs")
	}
	if !d.Requirements.AlwaysAvailable || len(d.Prompt.AllowedTools) != 1 || d.Prompt.AllowedTools[0] != ReadAttachment {
		t.Fatal("default attachment capability prompt missing")
	}
}
