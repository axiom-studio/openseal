package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestConversationRunLineageValueInheritsThreadPlacement(t *testing.T) {
	root := &models.AgentRun{ID: "root", RootRunID: "root", Context: map[string]interface{}{"threadRootMessageId": "thread", "triggerMessageId": "trigger"}}
	parent := &models.AgentRun{ID: "parent", RootRunID: "root", ParentRunID: "root", Context: map[string]interface{}{}}
	child := &models.AgentRun{ID: "child", RootRunID: "root", ParentRunID: "parent", Context: map[string]interface{}{"triggerMessageId": "own"}}
	runs := map[string]*models.AgentRun{"root": root, "parent": parent, "child": child}
	if got := conversationRunLineageValue(child, runs, "threadRootMessageId"); got != "thread" {
		t.Fatalf("thread = %q", got)
	}
	if got := conversationRunLineageValue(child, runs, "triggerMessageId"); got != "own" {
		t.Fatalf("trigger = %q", got)
	}
	if got := conversationRunLineageValue(&models.AgentRun{ID: "orphan", ParentRunID: "missing"}, runs, "threadRootMessageId"); got != "" {
		t.Fatalf("orphan = %q", got)
	}
}
