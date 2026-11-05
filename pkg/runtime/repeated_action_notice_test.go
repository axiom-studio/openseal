package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func repeatedActionHistory(entries ...map[string]interface{}) map[string]interface{} {
	values := make([]interface{}, len(entries))
	for index := range entries {
		values[index] = entries[index]
	}
	return map[string]interface{}{actionHistoryCheckpointKey: values}
}

func githubContentEntry(id, path, status string) map[string]interface{} {
	return map[string]interface{}{
		"actionCallId": id, "semanticDigest": "digest:" + path, "status": status,
		"skillId": "github", "action": "repository-content-get", "bindingId": "github-binding",
		"arguments": map[string]interface{}{"owner": "axiom-studio", "repo": "cortex", "path": path},
	}
}

var repeatedNoticeActions = []capability.ModelAction{{
	Name: "github-repository-content-get", SkillID: "github", Action: "repository-content-get", BindingID: "github-binding",
}}

func repeatedNoticeTurn(t *testing.T, conversation bool, checkpoint map[string]interface{}) []string {
	t.Helper()
	runner, err := NewHostedTurnRunner(&recordingTurnHost{}, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1", Actions: repeatedNoticeActions,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "67"}, AssignedAgentID: "agent", Goal: "Is the retire-agent fix in cortex?", Checkpoint: checkpoint}
	if conversation {
		run.Context = map[string]interface{}{"conversationId": "chat-1"}
	}
	request, err := runner.buildRequest(TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
	if err != nil {
		t.Fatal(err)
	}
	return request.SystemInstructions
}

func noticesContaining(instructions []string, text string) []string {
	var result []string
	for _, instruction := range instructions {
		if strings.Contains(instruction, text) {
			result = append(result, instruction)
		}
	}
	return result
}

func TestRepeatedActionNoticeNamesEarlierIdenticalResult(t *testing.T) {
	checkpoint := repeatedActionHistory(
		githubContentEntry("call-1", "pkg/a.go", "succeeded"),
		githubContentEntry("call-2", "pkg/b.go", "succeeded"),
		githubContentEntry("call-3", "pkg/a.go", "succeeded"),
	)
	notices := noticesContaining(repeatedNoticeTurn(t, true, checkpoint), "same arguments")
	if len(notices) != 1 || !strings.Contains(notices[0], "github-repository-content-get") || !strings.Contains(notices[0], "actionCallId call-1") ||
		!strings.Contains(notices[0], "_opensealActionHistory") {
		t.Fatalf("identical-call notice = %#v", notices)
	}
	// A repeat of a call that failed is a retry, not a duplicate result.
	failedFirst := repeatedActionHistory(githubContentEntry("call-1", "pkg/a.go", "failed"), githubContentEntry("call-2", "pkg/a.go", "succeeded"))
	if notices := noticesContaining(repeatedNoticeTurn(t, true, failedFirst), "same arguments"); len(notices) != 0 {
		t.Fatalf("retry after failure was flagged: %#v", notices)
	}
	// Only the latest call is judged, so an old repeat does not nag forever.
	moved := repeatedActionHistory(githubContentEntry("call-1", "pkg/a.go", "succeeded"),
		githubContentEntry("call-2", "pkg/a.go", "succeeded"), githubContentEntry("call-3", "pkg/c.go", "succeeded"))
	if notices := noticesContaining(repeatedNoticeTurn(t, true, moved), "same arguments"); len(notices) != 0 {
		t.Fatalf("stale repeat was flagged: %#v", notices)
	}
}

func TestRepeatedActionNoticeSendsGrindingToTheVM(t *testing.T) {
	entries := []map[string]interface{}{}
	for index := 0; index < repeatedActionThreshold; index++ {
		status := "succeeded"
		if index == 2 {
			status = "failed"
		}
		entries = append(entries, githubContentEntry(fmt.Sprintf("call-%d", index), fmt.Sprintf("pkg/%d.go", index), status))
	}
	instructions := repeatedNoticeTurn(t, true, repeatedActionHistory(entries...))
	notices := noticesContaining(instructions, "You've called")
	want := "You've called github-repository-content-get 6 times in your last 6 tool calls; for work like this, write a script in the secure VM (seal call github-repository-content-get …) or clone the repository with git.seal."
	if len(notices) != 1 || notices[0] != want {
		t.Fatalf("repeated-tool notice = %#v", notices)
	}
	// One call fewer stays silent.
	if notices := noticesContaining(repeatedNoticeTurn(t, true, repeatedActionHistory(entries[1:]...)), "You've called"); len(notices) != 0 {
		t.Fatalf("below threshold = %#v", notices)
	}
}

func TestRepeatedActionNoticeOmitsVMWithoutRunCommand(t *testing.T) {
	entries := []map[string]interface{}{}
	for index := 0; index < 8; index++ {
		entries = append(entries, githubContentEntry(fmt.Sprintf("call-%d", index), fmt.Sprintf("pkg/%d.go", index), "succeeded"))
	}
	notices := noticesContaining(repeatedNoticeTurn(t, false, repeatedActionHistory(entries...)), "You've called")
	if len(notices) != 1 || !strings.Contains(notices[0], "github-repository-content-get 8 times") ||
		strings.Contains(notices[0], "VM") || strings.Contains(notices[0], "seal call") || strings.Contains(notices[0], "git.seal") {
		t.Fatalf("notice without run_command = %#v", notices)
	}
}

func TestRepeatedActionNoticeNamesAtMostTwoBindingTools(t *testing.T) {
	actions := []capability.ModelAction{}
	entries := []map[string]interface{}{}
	for _, tool := range []struct {
		name  string
		count int
	}{{"search", 6}, {"fetch", 7}, {"list", 6}, {"get", 5}} {
		actions = append(actions, capability.ModelAction{
			Name: "mcp.docs." + tool.name, SkillID: "openseal.mcp", Action: "call_tool", BindingID: "docs",
			BindingTool: &capability.ModelActionBindingTool{Name: tool.name, InputArgument: "input", Arguments: map[string]interface{}{"tool": tool.name}},
		})
		for index := 0; index < tool.count; index++ {
			entries = append(entries, map[string]interface{}{
				"actionCallId": fmt.Sprintf("%s-%d", tool.name, index), "semanticDigest": fmt.Sprintf("%s-%d", tool.name, index), "status": "succeeded",
				"skillId": "openseal.mcp", "action": "call_tool", "bindingId": "docs",
				"arguments": map[string]interface{}{"tool": tool.name, "input": map[string]interface{}{"n": index}},
			})
		}
	}
	request := &HostedTurnRequest{Actions: actions, ContinuationCheckpoint: repeatedActionHistory(entries...)}
	projectRepeatedActionNotice(request)
	if len(request.SystemInstructions) != 2 ||
		!strings.HasPrefix(request.SystemInstructions[0], "You've called mcp.docs.fetch 7 times in your last 24 tool calls") ||
		!strings.HasPrefix(request.SystemInstructions[1], "You've called mcp.docs.list 6 times") {
		t.Fatalf("notices = %#v", request.SystemInstructions)
	}
}
