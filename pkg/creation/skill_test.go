package creation

import (
	"github.com/axiom-studio/openseal/pkg/skill"
	"testing"
)

func TestCreationActionsRequireRevision(t *testing.T) {
	for _, action := range []skill.Action{PageAction(), SheetAction()} {
		found := false
		for _, key := range action.InputSchema["required"].([]interface{}) {
			if key == "expectedLatestVersion" {
				found = true
			}
		}
		if !found {
			t.Fatal("missing concurrency precondition")
		}
	}
}
