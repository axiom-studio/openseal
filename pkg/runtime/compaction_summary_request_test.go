package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestCompactionSummaryRequestKeepsStepPrefix(t *testing.T) {
	f := newRenderFixture(t, "chat")
	f.user("run-1", "find the launch notes")
	early := addSurfacingLoad(f, "step-1", "c1", "action:notion.fetch", "action_notion_fetch")
	f.user("run-1", "and gmail")
	late := addSurfacingLoad(f, "step-2", "c2", "action:gmail.read", "action_gmail_read")
	step := surfacedModelStep(t, f.path())

	// A compaction carries the earlier compaction's tools, then the tools
	// of the loads it summarizes, each name once.
	if carried := carriedToolsAfter(nil, f.entries[:3]); !reflect.DeepEqual(carried, early.Tool.SurfacedTools) {
		t.Fatalf("carried after the first load = %+v", carried)
	}
	carried := carriedToolsAfter(&models.CompactionEntry{CarriedTools: []models.SurfacedTool{{Capability: "action:drive.list", Name: "action_drive_list",
		InputSchema: map[string]interface{}{"type": "object"}}}}, []*models.SessionEntry{early})
	if got := []string{carried[0].Name, carried[1].Name}; len(carried) != 2 || !reflect.DeepEqual(got, []string{"action_drive_list", "action_notion_fetch"}) {
		t.Fatalf("carried = %+v", carried)
	}

	// The whole-path summary is the step's request plus the instruction.
	whole := compactionSummaryRequest(step)
	if !reflect.DeepEqual(whole.Tools, step.Tools) || !reflect.DeepEqual(whole.Entries, step.Entries) || !reflect.DeepEqual(whole.System, step.System) {
		t.Fatal("the whole-path summary request changed the step's tools, entries or system")
	}
	if err := whole.Validate(); err != nil {
		t.Fatal(err)
	}

	at := func(id string) int {
		for i, entry := range step.Entries {
			if entry.ID == id {
				return i
			}
		}
		t.Fatalf("entry %s not rendered", id)
		return -1
	}
	surfaceOf := func(req models.ModelStepRequest, name string) (string, bool) {
		for _, tool := range req.Tools {
			if tool.Name == name {
				return tool.Surface, true
			}
		}
		return "", false
	}
	// A chunk ending at the first load: its tool keeps the surface, the
	// later load's tool is not declared.
	first := compactionChunkSummaryRequest(step, step.Entries[:at(early.ID)+1])
	if surface, ok := surfaceOf(first, "action_notion_fetch"); !ok || surface != early.ID {
		t.Fatalf("first chunk notion surface = %q %v", surface, ok)
	}
	if _, ok := surfaceOf(first, "action_gmail_read"); ok {
		t.Fatal("a tool surfaced after the chunk is declared")
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	// A chunk after the first load: its tool is callable from the start.
	second := compactionChunkSummaryRequest(step, step.Entries[at(early.ID)+1:])
	if surface, ok := surfaceOf(second, "action_notion_fetch"); !ok || surface != "" {
		t.Fatalf("second chunk notion surface = %q %v", surface, ok)
	}
	if surface, ok := surfaceOf(second, "action_gmail_read"); !ok || surface != late.ID {
		t.Fatalf("second chunk gmail surface = %q %v", surface, ok)
	}
}

// addSurfacingLoad appends a load_capabilities call and its completed entry
// that surfaced one tool, frozen as the tool executor freezes it.
func addSurfacingLoad(f *renderFixture, step, call, capabilityID, name string) *models.SessionEntry {
	arguments := json.RawMessage(`{"ids":["` + capabilityID + `"]}`)
	f.add(&models.SessionEntry{RunID: "run-1", StepID: step, Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
		Provider: models.ModelStepProviderAnthropic, Model: "claude-test", StopReason: models.ModelStopToolCalls,
		ToolCalls: []models.ModelToolCall{{ID: call, Name: LoadCapabilitiesTool, Arguments: arguments}},
	}})
	tool := models.ModelStepTool{Name: name, Description: "Read " + name, InputSchema: map[string]interface{}{"type": "object"}}
	return f.add(&models.SessionEntry{RunID: "run-1", StepID: step, Type: models.SessionEntryTool, State: models.ToolEntryCompleted, ToolCallID: call, Tool: &models.ToolEntry{
		Name: LoadCapabilitiesTool, Arguments: arguments, SideEffect: models.ToolSideEffectNone,
		SurfacedTools: []models.SurfacedTool{surfacedTool(capabilityID, tool)},
		Result:        &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{{Type: models.ContentPartText, Text: `{"status":"loaded"}`}}},
	}})
}

// surfacedModelStep is a step over path as the step preparer completes it:
// the rendered entries, and the base tools with the loaded group of the path.
func surfacedModelStep(t *testing.T, path *models.AgentSessionPath) models.ModelStepRequest {
	t.Helper()
	step := validStepRequest()
	entries, err := projectSessionPath(context.Background(), path, sessionRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	load := models.ModelStepTool{Name: LoadCapabilitiesTool, Description: "Load.", InputSchema: map[string]interface{}{"type": "object"}, Group: models.ModelStepToolHarness}
	step.Tools = applySurfaces(append([]models.ModelStepTool{step.Tools[0], load}, step.Tools[1]), path, entries)
	step.Entries = entries
	if err := step.Validate(); err != nil {
		t.Fatalf("surfaced step does not validate: %v", err)
	}
	return step
}
