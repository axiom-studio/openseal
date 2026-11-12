package runtime

import "testing"

func TestConversationRunLineageValueInheritsThreadPlacement(t *testing.T) {
	root := &AgentRun{ID: "root", RootRunID: "root", Context: map[string]interface{}{"threadRootMessageId": "thread", "triggerMessageId": "trigger"}}
	parent := &AgentRun{ID: "parent", RootRunID: "root", ParentRunID: "root", Context: map[string]interface{}{}}
	child := &AgentRun{ID: "child", RootRunID: "root", ParentRunID: "parent", Context: map[string]interface{}{"triggerMessageId": "own"}}
	runs := map[string]*AgentRun{"root": root, "parent": parent, "child": child}
	if got := conversationRunLineageValue(child, runs, "threadRootMessageId"); got != "thread" {
		t.Fatalf("thread = %q", got)
	}
	if got := conversationRunLineageValue(child, runs, "triggerMessageId"); got != "own" {
		t.Fatalf("trigger = %q", got)
	}
	if got := conversationRunLineageValue(&AgentRun{ID: "orphan", ParentRunID: "missing"}, runs, "threadRootMessageId"); got != "" {
		t.Fatalf("orphan = %q", got)
	}
}
