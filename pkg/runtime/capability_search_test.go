package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

var capabilitySearchFixtureTools = []string{
	"search_pages", "create_page", "update_page", "delete_page", "list_databases", "query_database", "create_database",
	"get_user", "list_users", "add_comment", "list_comments", "get_block", "append_block", "delete_block",
	"move_page", "duplicate_page", "export_page", "share_page", "archive_page", "restore_page",
}

// capabilitySearchFixture is an Agent with servers connected MCP servers of
// toolsPerServer binding tools each, named like the kernel names them.
func capabilitySearchFixture(servers, toolsPerServer int) CapabilityCatalog {
	catalog := CapabilityCatalog{Scope: Scope{Kind: "tenant", ID: "11"}, AgentID: "agent"}
	for server := 0; server < servers; server++ {
		slug := fmt.Sprintf("svc%04d", server)
		for tool := 0; tool < toolsPerServer; tool++ {
			name := capabilitySearchFixtureTools[tool%len(capabilitySearchFixtureTools)]
			verb, object, _ := strings.Cut(name, "_")
			catalog.Actions = append(catalog.Actions, capability.ModelAction{
				Name: "mcp." + slug + "." + name, Description: fmt.Sprintf("%s a %s in Service %d.", strings.ToUpper(verb[:1])+verb[1:], object, server),
				SkillID: "openseal.mcp", Version: "1.0.0", BindingID: slug, BindingRevision: 1, Action: "read",
				SideEffect: capability.SideEffectRead, Risk: capability.RiskLevelRead,
				InputSchema: map[string]interface{}{"type": "object"},
				BindingTool: &capability.ModelActionBindingTool{Name: name, InputArgument: "arguments"},
			})
		}
	}
	return catalog
}

func searchArgs(query string) map[string]interface{} {
	return map[string]interface{}{"query": query}
}

func TestSearchCapabilitiesRanksExactNameFirst(t *testing.T) {
	s := NewCapabilitySet(capabilitySearchFixture(100, 20), nil, CapabilitySetConfig{})
	if !s.Searching() {
		t.Fatal("expected search mode")
	}
	content, isError := ExecuteSearchCapabilities(s, map[string]interface{}{"query": "svc0042 search pages"}, 0)
	fields := decodeCapabilityContent(t, content)
	results, _ := fields["results"].([]interface{})
	if isError || fields["status"] != "search_results" || len(results) != capabilitySearchDefaultLimit ||
		results[0].(map[string]interface{})["id"] != "action:mcp.svc0042.search_pages" {
		t.Fatalf("search = %s", content)
	}
	first := results[0].(map[string]interface{})
	if first["kind"] != "action" || first["loaded"] != false || !strings.Contains(first["description"].(string), "Service 42") {
		t.Fatalf("result = %v", first)
	}
	// Description match and explicit limit (JSON numbers decode as float64).
	content, _ = ExecuteSearchCapabilities(s, map[string]interface{}{"query": "Service 99 export", "limit": float64(3)}, 0)
	results = decodeCapabilityContent(t, content)["results"].([]interface{})
	if len(results) != 3 || results[0].(map[string]interface{})["id"] != "action:mcp.svc0099.export_page" {
		t.Fatalf("description search = %s", content)
	}
	// Search never loads anything.
	if s.Loaded("action:mcp.svc0042.search_pages") {
		t.Fatal("search loaded a capability")
	}
	for _, args := range []map[string]interface{}{
		{}, {"query": ""}, {"query": "  "}, {"query": "x", "limit": float64(21)}, {"query": "x", "limit": float64(0)},
		{"query": "x", "limit": 1.5}, {"query": "x", "extra": 1}, {"query": strings.Repeat("q", 201)}, {"query": 7},
	} {
		content, isError := ExecuteSearchCapabilities(s, args, 0)
		if !isError || decodeCapabilityContent(t, content)["status"] != "invalid_arguments" {
			t.Fatalf("invalid search %v accepted: %s", args, content)
		}
	}
}

func TestSearchCapabilitiesMarksLoaded(t *testing.T) {
	catalog := capabilitySearchFixture(100, 20)
	s := NewCapabilitySet(catalog, []string{"action:mcp.svc0001.get_user"}, CapabilitySetConfig{})
	content, _ := ExecuteSearchCapabilities(s, map[string]interface{}{"query": "svc0001 get user", "limit": 2}, 0)
	results := decodeCapabilityContent(t, content)["results"].([]interface{})
	if len(results) != 2 || results[0].(map[string]interface{})["id"] != "action:mcp.svc0001.get_user" ||
		results[0].(map[string]interface{})["loaded"] != true || results[1].(map[string]interface{})["loaded"] != false {
		t.Fatalf("loaded flags = %s", content)
	}
	// Loading a result in the same step marks it for a later search.
	if _, isError, _ := ExecuteLoadCapabilities(s, loadArgs("action:mcp.svc0001.list_users"), capabilityToolName); isError {
		t.Fatal("load after search failed")
	}
	content, _ = ExecuteSearchCapabilities(s, map[string]interface{}{"query": "svc0001 list users", "limit": 1}, 1)
	if r := decodeCapabilityContent(t, content)["results"].([]interface{})[0].(map[string]interface{}); r["loaded"] != true {
		t.Fatalf("after load = %s", content)
	}
}

func TestSearchCapabilitiesLimitEightPerRun(t *testing.T) {
	s := NewCapabilitySet(capabilitySearchFixture(100, 20), nil, CapabilitySetConfig{})
	var path []*SessionEntry
	for i := 0; i < MaxCapabilitySearchesPerRun; i++ {
		count := CapabilitySearchesThisRun(path, "run-1")
		if count != i {
			t.Fatalf("count = %d, want %d", count, i)
		}
		content, isError := ExecuteSearchCapabilities(s, searchArgs("page"), count)
		if isError || decodeCapabilityContent(t, content)["status"] != "search_results" {
			t.Fatalf("search %d = %s", i, content)
		}
		path = append(path, &SessionEntry{ID: fmt.Sprintf("s%d", i), RunID: "run-1", Type: SessionEntryTool, State: ToolEntryCompleted,
			Tool: &ToolEntry{Name: SearchCapabilitiesTool}})
	}
	// Errored searches and other Runs' searches do not count.
	path = append(path,
		&SessionEntry{ID: "err", RunID: "run-1", Type: SessionEntryTool, State: ToolEntryError, Tool: &ToolEntry{Name: SearchCapabilitiesTool}},
		&SessionEntry{ID: "other", RunID: "run-0", Type: SessionEntryTool, State: ToolEntryCompleted, Tool: &ToolEntry{Name: SearchCapabilitiesTool}},
		capabilityLoadEntry("load", "run-1", ToolEntryCompleted, "action:mcp.svc0001.get_user"))
	count := CapabilitySearchesThisRun(path, "run-1")
	if count != MaxCapabilitySearchesPerRun {
		t.Fatalf("count = %d", count)
	}
	content, isError := ExecuteSearchCapabilities(s, searchArgs("page"), count)
	fields := decodeCapabilityContent(t, content)
	if isError || fields["status"] != "search_limit_reached" || fields["results"] != nil ||
		!strings.Contains(fields["next"].(string), "load_capabilities") {
		t.Fatalf("past the limit = %s", content)
	}
	if fresh := CapabilitySearchesThisRun(path, "run-2"); fresh != 0 {
		t.Fatalf("a new Run starts with %d searches", fresh)
	}
}

func TestSearchCapabilitiesNoMatch(t *testing.T) {
	s := NewCapabilitySet(capabilitySearchFixture(100, 20), nil, CapabilitySetConfig{})
	for _, query := range []string{"zzzz qqqq", "the and of"} {
		content, isError := ExecuteSearchCapabilities(s, searchArgs(query), 0)
		fields := decodeCapabilityContent(t, content)
		results, ok := fields["results"].([]interface{})
		if isError || fields["status"] != "no_match" || !ok || len(results) != 0 {
			t.Fatalf("no match %q = %s", query, content)
		}
	}
	// A catalog that lists everything still offers search, so the tool
	// list does not change when the catalog crosses the budget.
	listed := NewCapabilitySet(capabilityCatalogFixture(), nil, CapabilitySetConfig{})
	if listed.Searching() {
		t.Fatal("fixture must list its catalog")
	}
	if content, isError := ExecuteSearchCapabilities(listed, searchArgs("notion"), 0); isError {
		t.Fatalf("search of a listed catalog = %s", content)
	}
	// A small catalog is deferred and searchable too.
	small := NewCapabilitySet(CapabilityCatalog{Actions: capabilityCatalogFixture().Actions[:1]}, nil, CapabilitySetConfig{})
	if content, isError := ExecuteSearchCapabilities(small, searchArgs("notion"), 0); isError {
		t.Fatalf("search of a small catalog = %s", content)
	}
	if _, isError := ExecuteSearchCapabilities(nil, searchArgs("notion"), 0); !isError {
		t.Fatal("nil set must refuse search")
	}
}

func TestSearchCapabilitiesAtCatalogScale(t *testing.T) {
	s := NewCapabilitySet(capabilitySearchFixture(2000, 20), nil, CapabilitySetConfig{})
	content, isError := ExecuteSearchCapabilities(s, map[string]interface{}{"query": "svc1234 create page", "limit": 5}, 0)
	results := decodeCapabilityContent(t, content)["results"].([]interface{})
	if isError || len(results) != 5 || results[0].(map[string]interface{})["id"] != "action:mcp.svc1234.create_page" {
		t.Fatalf("results = %s", content)
	}
	for _, result := range results {
		if !strings.Contains(result.(map[string]interface{})["id"].(string), "svc1234") {
			t.Fatalf("ranked an unrelated server: %v", results)
		}
	}
	if len(content) > MaxToolResultContentBytes {
		t.Fatalf("content is %d bytes", len(content))
	}
}
