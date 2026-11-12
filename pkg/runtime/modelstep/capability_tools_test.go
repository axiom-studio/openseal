package modelstep_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// capabilityToolName names a tool the way a tool-set builder would.
func capabilityToolName(id string) string {
	_, name, ok := strings.Cut(id, ":")
	if !ok || strings.HasPrefix(id, modelstep.CapabilityPromptPrefix) {
		return ""
	}
	return strings.NewReplacer(".", "_").Replace(name)
}

func decodeCapabilityContent(t *testing.T, content json.RawMessage) map[string]interface{} {
	t.Helper()
	var fields map[string]interface{}
	if err := json.Unmarshal(content, &fields); err != nil {
		t.Fatalf("content %s: %v", content, err)
	}
	return fields
}

func loadArgs(ids ...string) map[string]interface{} {
	values := make([]interface{}, len(ids))
	for i, id := range ids {
		values[i] = id
	}
	return map[string]interface{}{"ids": values}
}

// oneAtATime turns off whole-Skill loads: no sibling fits the budget.
var oneAtATime = modelstep.CapabilitySetConfig{SkillLoadBudget: 1}

func TestLoadCapabilitiesLoadsAndNamesTools(t *testing.T) {
	s := modelstep.NewCapabilitySet(modelsteptest.CapabilityCatalogFixture(), nil, oneAtATime)
	content, isError, loaded := modelstep.ExecuteLoadCapabilities(s, loadArgs("action:notion.op1", "action:notion.op1", "action:notion.op2"), capabilityToolName)
	if isError {
		t.Fatalf("load failed: %s", content)
	}
	want := []string{"prompt:skill-notion@3", "action:notion.op1", "action:notion.op2"}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded = %v, want %v", loaded, want)
	}
	fields := decodeCapabilityContent(t, content)
	if fields["status"] != "loaded" || fields["next"] != modelstep.CapabilityLoadedNext {
		t.Fatalf("content = %s", content)
	}
	items := fields["loaded"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	prompt := items[0].(map[string]interface{})
	if prompt["id"] != "prompt:skill-notion@3" || prompt["name"] != "Notion" || prompt["instructions"] != "Use Notion carefully." || prompt["tool"] != nil {
		t.Fatalf("prompt item = %v", prompt)
	}
	action := items[1].(map[string]interface{})
	if action["id"] != "action:notion.op1" || action["tool"] != "notion_op1" || action["summary"] != "Notion operation 1" || action["instructions"] != nil {
		t.Fatalf("action item = %v", action)
	}
	if !s.Loaded("action:notion.op1") || !s.Loaded("prompt:skill-notion@3") {
		t.Fatal("the set was not updated")
	}

	// The next step derives the same set from the completed entry.
	next := modelstep.NewCapabilitySet(modelsteptest.CapabilityCatalogFixture(), modelstep.PathLoadedCapabilityIDs(&models.AgentSessionPath{Entries: []*models.SessionEntry{
		capabilityLoadEntry("e1", "run-1", models.ToolEntryCompleted, loaded...),
	}}), oneAtATime)
	if !reflect.DeepEqual(next.IDs(), s.IDs()) {
		t.Fatalf("next step ids %v != %v", next.IDs(), s.IDs())
	}
}

func TestLoadCapabilitiesAlreadyLoadedIsNoop(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	s := modelstep.NewCapabilitySet(catalog, []string{"action:notion.op1"}, oneAtATime)
	before := s.IDs()
	readFile := "workspace:" + workspace.OperationReadFile
	args := loadArgs("action:notion.op1", readFile)
	content, isError, loaded := modelstep.ExecuteLoadCapabilities(s, args, capabilityToolName)
	if isError || len(loaded) != 0 {
		t.Fatalf("no-op load: isError=%v loaded=%v %s", isError, loaded, content)
	}
	fields := decodeCapabilityContent(t, content)
	if fields["status"] != "already_loaded" || fields["loaded"] != nil {
		t.Fatalf("content = %s", content)
	}
	already := fields["alreadyLoaded"].([]interface{})
	first := already[0].(map[string]interface{})
	second := already[1].(map[string]interface{})
	if first["id"] != "action:notion.op1" || !reflect.DeepEqual(first["tools"], []interface{}{"notion_op1"}) ||
		second["id"] != readFile || !reflect.DeepEqual(second["tools"], []interface{}{"workspace_read_file"}) {
		t.Fatalf("alreadyLoaded = %v", already)
	}
	if !reflect.DeepEqual(s.IDs(), before) {
		t.Fatal("a no-op load changed the set")
	}
	// A repeat returns byte-identical content (D9 read-repeat counting).
	again, _, _ := modelstep.ExecuteLoadCapabilities(s, args, capabilityToolName)
	if string(again) != string(content) {
		t.Fatalf("repeat content differs:\n%s\n%s", again, content)
	}
	// Mixed: the new ID loads and the old one is listed.
	content, isError, loaded = modelstep.ExecuteLoadCapabilities(s, loadArgs("action:notion.op1", "action:notion.op5"), capabilityToolName)
	fields = decodeCapabilityContent(t, content)
	if isError || !reflect.DeepEqual(loaded, []string{"action:notion.op5"}) || fields["status"] != "loaded" || fields["alreadyLoaded"] == nil {
		t.Fatalf("mixed load: %v %s", loaded, content)
	}
}

func TestLoadCapabilitiesUnknownIDLoadsNothing(t *testing.T) {
	s := modelstep.NewCapabilitySet(modelsteptest.CapabilityCatalogFixture(), nil, modelstep.CapabilitySetConfig{})
	before := s.IDs()
	content, isError, loaded := modelstep.ExecuteLoadCapabilities(s, loadArgs("action:notion.op1", "notion.op2", "action:missing"), capabilityToolName)
	if !isError || len(loaded) != 0 {
		t.Fatalf("unknown load: isError=%v loaded=%v", isError, loaded)
	}
	fields := decodeCapabilityContent(t, content)
	if fields["status"] != "not_available" || fields["error"] != modelstep.CapabilityLookupUnavailableText ||
		!reflect.DeepEqual(fields["ids"], []interface{}{"notion.op2", "action:missing"}) {
		t.Fatalf("content = %s", content)
	}
	if !reflect.DeepEqual(s.IDs(), before) {
		t.Fatal("a rejected load changed the set")
	}
	for _, args := range []map[string]interface{}{
		{},
		{"ids": []interface{}{}},
		{"ids": []interface{}{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		{"ids": []interface{}{" "}},
		{"ids": []interface{}{1}},
		{"ids": "action:notion.op1"},
		{"ids": []interface{}{"action:notion.op1"}, "extra": true},
	} {
		content, isError, loaded := modelstep.ExecuteLoadCapabilities(s, args, capabilityToolName)
		if !isError || len(loaded) != 0 || decodeCapabilityContent(t, content)["status"] != "invalid_arguments" {
			t.Fatalf("invalid arguments %v accepted: %s", args, content)
		}
	}
}

func TestLoadCapabilitiesResolvesDiscoveryHandles(t *testing.T) {
	s := modelstep.NewCapabilitySet(longPromptCatalogFixture(), nil, modelstep.CapabilitySetConfig{})
	canonical := "prompt:" + longPromptCatalogFixture().SkillPrompts[0].Reference
	handle := s.DiscoveryID(canonical)
	if handle == canonical {
		t.Fatal("fixture must produce a handle")
	}
	content, isError, loaded := modelstep.ExecuteLoadCapabilities(s, loadArgs(handle, canonical), capabilityToolName)
	if isError || !reflect.DeepEqual(loaded, []string{canonical}) {
		t.Fatalf("handle load: %v %s", loaded, content)
	}
	item := decodeCapabilityContent(t, content)["loaded"].([]interface{})[0].(map[string]interface{})
	if item["id"] != handle {
		t.Fatalf("result must show the handle the model sees: %v", item)
	}
}

func TestLoadCapabilitiesInstructionsInResultNotListing(t *testing.T) {
	s := modelstep.NewCapabilitySet(modelsteptest.CapabilityCatalogFixture(), nil, modelstep.CapabilitySetConfig{})
	listing := modelsteptest.CapabilityNoteLines(s.CatalogLines())
	content, isError, _ := modelstep.ExecuteLoadCapabilities(s, loadArgs("prompt:skill-research-2@1"), capabilityToolName)
	if isError || !strings.Contains(string(content), "Research instructions 2.") {
		t.Fatalf("instructions missing from result: %s", content)
	}
	if strings.Contains(listing, "Research instructions 2.") || strings.Contains(modelstep.CapabilityLoadingText, "Research instructions 2.") {
		t.Fatal("loaded instructions must travel in the tool result only")
	}
	next := modelstep.NewCapabilitySet(modelsteptest.CapabilityCatalogFixture(), []string{"prompt:skill-research-2@1"}, modelstep.CapabilitySetConfig{})
	if modelsteptest.CapabilityNoteLines(next.CatalogLines()) != listing {
		t.Fatal("a load changed the listing")
	}
}

func TestLoadCapabilitiesCapsContentAndReportsNotLoaded(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	catalog.SkillPrompts[0].Instructions = strings.Repeat("a", 40<<10)
	catalog.SkillPrompts[1].Instructions = strings.Repeat("b", 40<<10)
	catalog.SkillPrompts[2].Instructions = strings.Repeat("é", 50<<10) // 100 KiB on its own
	s := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})

	content, isError, loaded := modelstep.ExecuteLoadCapabilities(s, loadArgs("prompt:skill-research-0@1", "prompt:skill-research-1@1", "action:notion.op0"), capabilityToolName)
	if isError || len(content) > models.MaxToolResultContentBytes {
		t.Fatalf("capped load: isError=%v len=%d", isError, len(content))
	}
	if !reflect.DeepEqual(loaded, []string{"prompt:skill-research-0@1"}) {
		t.Fatalf("loaded = %v", loaded)
	}
	fields := decodeCapabilityContent(t, content)
	notLoaded := fields["notLoaded"].([]interface{})
	if len(notLoaded) != 2 || notLoaded[0].(map[string]interface{})["id"] != "prompt:skill-research-1@1" ||
		notLoaded[1].(map[string]interface{})["reason"] != modelstep.CapabilityNotLoadedReason {
		t.Fatalf("notLoaded = %v", notLoaded)
	}
	if !strings.Contains(fields["next"].(string), "notLoaded") {
		t.Fatalf("next = %v", fields["next"])
	}
	if s.Loaded("prompt:skill-research-1@1") || s.Loaded("action:notion.op0") {
		t.Fatal("not-loaded IDs became visible")
	}

	// A single prompt too large on its own is loaded with its instructions cut.
	content, isError, loaded = modelstep.ExecuteLoadCapabilities(s, loadArgs("prompt:skill-research-2@1"), capabilityToolName)
	if isError || len(content) > models.MaxToolResultContentBytes || !reflect.DeepEqual(loaded, []string{"prompt:skill-research-2@1"}) {
		t.Fatalf("truncated load: isError=%v len=%d loaded=%v", isError, len(content), loaded)
	}
	item := decodeCapabilityContent(t, content)["loaded"].([]interface{})[0].(map[string]interface{})
	instructions := item["instructions"].(string)
	if item["instructionsTruncated"] != true || len(instructions) < 30<<10 || !strings.HasPrefix(catalog.SkillPrompts[2].Instructions, instructions) {
		t.Fatalf("truncated item: truncated=%v len=%d", item["instructionsTruncated"], len(instructions))
	}
}

func TestLoadCapabilitiesSchemaByteStable(t *testing.T) {
	const wantSchema = `{"additionalProperties":false,"properties":{"ids":{"items":{"description":"An exact capability ID, including its prefix.","type":"string"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true}},"required":["ids"],"type":"object"}`
	spec := modelstep.LoadCapabilitiesSpec()
	encoded, _ := json.Marshal(spec.InputSchema)
	if string(encoded) != wantSchema {
		t.Fatalf("load schema = %s", encoded)
	}
	if spec.Name != modelstep.LoadCapabilitiesTool || spec.Group != models.ModelStepToolHarness || !strings.Contains(spec.Description, "next step") {
		t.Fatalf("load spec = %+v", spec)
	}
	if strings.Contains(spec.Description, "turn") {
		t.Fatalf("load description still says turn: %q", spec.Description)
	}
	first, _ := json.Marshal(spec)
	second, _ := json.Marshal(modelstep.LoadCapabilitiesSpec())
	if string(first) != string(second) {
		t.Fatal("load spec is not byte-stable")
	}
	search := modelstep.SearchCapabilitiesSpec()
	encoded, _ = json.Marshal(search.InputSchema)
	const wantSearch = `{"additionalProperties":false,"properties":{"limit":{"description":"Maximum results; default 8.","maximum":20,"minimum":1,"type":"integer"},"query":{"description":"A few keywords: the service, the object and the verb.","maxLength":200,"minLength":1,"type":"string"}},"required":["query"],"type":"object"}`
	if string(encoded) != wantSearch || search.Name != modelstep.SearchCapabilitiesTool || search.Group != models.ModelStepToolHarness {
		t.Fatalf("search spec = %s %+v", encoded, search)
	}
}
