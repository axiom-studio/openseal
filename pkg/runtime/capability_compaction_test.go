package runtime

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TestCompactionCarriesSurfacedTools(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	early := f.addLoad("step-1", "c1", "action:notion.fetch")
	f.render.User("run-1", "and gmail")
	late := f.addLoad("step-2", "c2", "action:gmail.read")
	before := f.prepare("step-3")

	// Summarize up to the second load: the first load's tool is carried.
	kept := late.ID
	f.render.Add(&SessionEntry{RunID: "run-1", Type: SessionEntryCompaction, Compaction: &CompactionEntry{
		Summary: "Earlier work.", FirstKeptEntryID: f.render.Entries[len(f.render.Entries)-3].ID, TokensBefore: 9000,
		CarriedTools: early.Tool.SurfacedTools,
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
	set := f.preparer.CapabilitySet(agentrun.StepInput{Run: f.run, Binding: f.binding, Path: f.render.Path()})
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
