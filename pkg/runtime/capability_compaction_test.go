package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompactionCarriesSurfacedTools(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	early := f.addLoad("step-1", "c1", "action:notion.fetch")
	f.render.user("run-1", "and gmail")
	late := f.addLoad("step-2", "c2", "action:gmail.read")
	before := f.prepare("step-3")

	carried := carriedToolsAfter(&CompactionEntry{CarriedTools: []SurfacedTool{{Capability: "action:drive.list", Name: "action_drive_list",
		InputSchema: map[string]interface{}{"type": "object"}}}}, []*SessionEntry{early})
	if got := []string{carried[0].Name, carried[1].Name}; len(carried) != 2 || !reflect.DeepEqual(got, []string{"action_drive_list", "action_notion_fetch"}) {
		t.Fatalf("carried = %+v", carried)
	}

	// Summarize up to the second load: the first load's tool is carried.
	kept := late.ID
	f.render.add(&SessionEntry{RunID: "run-1", Type: SessionEntryCompaction, Compaction: &CompactionEntry{
		Summary: "Earlier work.", FirstKeptEntryID: f.render.entries[len(f.render.entries)-3].ID, TokensBefore: 9000,
		CarriedTools: carriedToolsAfter(nil, f.render.entries[:len(f.render.entries)-3]),
		Provider:     "anthropic", Model: "claude-test", Trigger: CompactionTriggerThreshold,
	}})
	after := f.prepare("step-3")
	loadedTools := func(req ModelStepRequest) []ModelStepTool {
		var tools []ModelStepTool
		for _, tool := range req.Tools {
			if tool.Group == ModelStepToolLoaded {
				tools = append(tools, tool)
			}
		}
		return tools
	}
	got := loadedTools(after)
	if len(got) != 2 || got[0].Name != "action_notion_fetch" || got[0].Surface != "" || got[1].Name != "action_gmail_read" || got[1].Surface != kept {
		t.Fatalf("loaded after compaction = %+v", got)
	}
	if mustJSON(t, got[0].InputSchema) != mustJSON(t, loadedTools(before)[0].InputSchema) {
		t.Fatal("the carried definition changed")
	}
	set := f.preparer.CapabilitySet(StepInput{Run: f.run, Binding: f.binding, Path: f.render.path()})
	if !set.Loaded("action:notion.fetch") {
		t.Fatal("a carried tool is not callable")
	}
	// The carried tool's bound instructions were in the summarized load
	// result: they are not loaded, and loading them returns their text.
	if set.Loaded("prompt:skill-notion@1") {
		t.Fatal("a carried tool's instructions count as loaded after the compaction")
	}
	content, isError, _ := ExecuteLoadCapabilities(set, map[string]interface{}{"ids": []interface{}{"prompt:skill-notion@1"}}, loopCapabilityToolName(f.binding.ModelActions))
	if isError || !strings.Contains(string(content), "Notion rules.") {
		t.Fatalf("reloading the carried tool's instructions = %v %s", isError, content)
	}
}

func TestCompactionSummaryRequestKeepsStepPrefix(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	early := f.addLoad("step-1", "c1", "action:notion.fetch")
	f.render.user("run-1", "and gmail")
	late := f.addLoad("step-2", "c2", "action:gmail.read")
	step := f.prepare("step-3")

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
	surfaceOf := func(req ModelStepRequest, name string) (string, bool) {
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
