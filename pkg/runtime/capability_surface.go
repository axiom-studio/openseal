package runtime

import (
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Surfaced tools (cache-stable capability loading). A load_capabilities call
// freezes the definitions of the tools it makes callable on its tool entry
// (ToolEntry.SurfacedTools). Every later step declares exactly those bytes in
// the loaded group, tagged with the entry they surfaced at
// (ModelStepTool.Surface, SessionEntryView.Surfaces), so a load only appends
// and a catalog change never rewrites a tool the model already has. A
// compaction that summarizes a load entry carries its tools over
// (CompactionEntry.CarriedTools); they are then callable from the start.

// SurfacedLoad is the tools that became callable at one entry of the path.
// EntryID "" holds the tools carried over the latest compaction.
type SurfacedLoad struct {
	EntryID string
	Tools   []models.SurfacedTool
}

// surfacedTool freezes a step tool definition.
func surfacedTool(capabilityID string, tool models.ModelStepTool) models.SurfacedTool {
	return models.SurfacedTool{
		Capability: capabilityID, Name: tool.Name, Description: tool.Description,
		InputSchema: models.CloneJSONObject(tool.InputSchema),
	}
}

// SurfacedOnPath is every tool callable on the path beyond harness and core:
// the latest compaction's carried tools (EntryID ""), then each completed
// load_capabilities entry in path order. A tool name surfaces once; its first
// surface wins.
func SurfacedOnPath(path *models.AgentSessionPath) []SurfacedLoad {
	if path == nil {
		return nil
	}
	seen := map[string]bool{}
	take := func(tools []models.SurfacedTool) []models.SurfacedTool {
		var kept []models.SurfacedTool
		for _, tool := range tools {
			if tool.Name == "" || seen[tool.Name] {
				continue
			}
			seen[tool.Name] = true
			kept = append(kept, tool)
		}
		return kept
	}
	var loads []SurfacedLoad
	if path.Compaction != nil && path.Compaction.Compaction != nil {
		if tools := take(path.Compaction.Compaction.CarriedTools); len(tools) > 0 {
			loads = append(loads, SurfacedLoad{Tools: tools})
		}
	}
	for _, entry := range path.Entries {
		if !completedLoadEntry(entry) {
			continue
		}
		if tools := take(entry.Tool.SurfacedTools); len(tools) > 0 {
			loads = append(loads, SurfacedLoad{EntryID: entry.ID, Tools: tools})
		}
	}
	return loads
}

func completedLoadEntry(entry *models.SessionEntry) bool {
	return entry != nil && entry.Type == models.SessionEntryTool && entry.Tool != nil &&
		entry.Tool.Name == LoadCapabilitiesTool && entry.State == models.ToolEntryCompleted
}

// PathCarriedCapabilityIDs is the capabilities of the tools carried over the
// path's latest compaction, in carried order.
func PathCarriedCapabilityIDs(path *models.AgentSessionPath) []string {
	if path == nil || path.Compaction == nil || path.Compaction.Compaction == nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, tool := range path.Compaction.Compaction.CarriedTools {
		if id := strings.TrimSpace(tool.Capability); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// PathLoadedCapabilityIDs is what the completed loads on the path loaded
// beyond the pinned set, in first-load order: their instructions and runbooks
// and the capabilities of the tools they surfaced. Tools carried over a
// compaction are not in it (PathCarriedCapabilityIDs). NewCapabilitySet takes
// it as loadedIDs.
func PathLoadedCapabilityIDs(path *models.AgentSessionPath) []string {
	if path == nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, entry := range path.Entries {
		if !completedLoadEntry(entry) {
			continue
		}
		for _, id := range entry.Tool.LoadedInstructions {
			add(id)
		}
		for _, tool := range entry.Tool.SurfacedTools {
			add(tool.Capability)
		}
	}
	return ids
}

// carriedToolsAfter is the CarriedTools of a compaction that summarizes
// summarized after previous: the previous carried tools, then the surfaced
// tools of the summarized load entries, each name once.
func carriedToolsAfter(previous *models.CompactionEntry, summarized []*models.SessionEntry) []models.SurfacedTool {
	var carried []models.SurfacedTool
	seen := map[string]bool{}
	add := func(tools []models.SurfacedTool) {
		for _, tool := range tools {
			if tool.Name != "" && !seen[tool.Name] {
				seen[tool.Name] = true
				carried = append(carried, tool)
			}
		}
	}
	if previous != nil {
		add(previous.CarriedTools)
	}
	for _, entry := range summarized {
		if completedLoadEntry(entry) {
			add(entry.Tool.SurfacedTools)
		}
	}
	return models.CloneSurfacedTools(carried)
}

// applySurfaces completes the step's tools with the loaded group of the
// rendered path: tools carried over the compaction (and the rare load whose
// entry did not render) are callable from the start; every other surfaced
// tool carries the id of its load entry, which lists it in Surfaces. base
// holds the harness and core tools; a surfaced name they already declare is
// left out. Surfaced tools follow their entries' render order.
func applySurfaces(base []models.ModelStepTool, path *models.AgentSessionPath, entries []models.SessionEntryView) []models.ModelStepTool {
	tools := make([]models.ModelStepTool, 0, len(base))
	taken := map[string]bool{}
	for _, tool := range base {
		if tool.Group == models.ModelStepToolLoaded {
			continue
		}
		tools = append(tools, tool)
		taken[tool.Name] = true
	}
	for i := range entries {
		entries[i].Surfaces = nil
	}
	position := map[string]int{}
	for i, entry := range entries {
		if entry.Type == models.SessionEntryTool && entry.Tool != nil && entry.Tool.State == models.ToolEntryCompleted {
			position[entry.ID] = i
		}
	}
	var carried []models.SurfacedTool
	var surfacing []SurfacedLoad
	for _, load := range SurfacedOnPath(path) {
		if _, ok := position[load.EntryID]; load.EntryID == "" || !ok {
			carried = append(carried, load.Tools...)
			continue
		}
		surfacing = append(surfacing, load)
	}
	sort.SliceStable(surfacing, func(i, j int) bool {
		return position[surfacing[i].EntryID] < position[surfacing[j].EntryID]
	})
	for _, tool := range carried {
		if !taken[tool.Name] {
			taken[tool.Name] = true
			tools = append(tools, tool.ModelStepTool(""))
		}
	}
	for _, load := range surfacing {
		at := position[load.EntryID]
		for _, tool := range load.Tools {
			if taken[tool.Name] {
				continue
			}
			taken[tool.Name] = true
			tools = append(tools, tool.ModelStepTool(load.EntryID))
			entries[at].Surfaces = append(entries[at].Surfaces, tool.Name)
		}
	}
	return tools
}

// chunkSurfaces fits the step's tools to a request over chunk, a span of the
// step's path (pathEntries): a surfaced tool whose entry is in chunk keeps its
// surface, one whose entry lies before the chunk becomes callable from the
// start, and one whose entry lies after it is dropped. Chunk entries keep only
// the surfaces of tools still declared.
func chunkSurfaces(tools []models.ModelStepTool, pathEntries, chunk []models.SessionEntryView) ([]models.ModelStepTool, []models.SessionEntryView) {
	position := make(map[string]int, len(pathEntries))
	for i, entry := range pathEntries {
		position[entry.ID] = i
	}
	present := make(map[string]bool, len(chunk))
	last := -1
	for _, entry := range chunk {
		present[entry.ID] = true
		if at, ok := position[entry.ID]; ok && at > last {
			last = at
		}
	}
	out := make([]models.ModelStepTool, 0, len(tools))
	declared := map[string]bool{}
	for _, tool := range tools {
		if tool.Surface != "" && !present[tool.Surface] {
			if at, ok := position[tool.Surface]; !ok || at > last {
				continue
			}
			tool.Surface = ""
		}
		if tool.Surface != "" {
			declared[tool.Name] = true
		}
		out = append(out, tool)
	}
	views := make([]models.SessionEntryView, len(chunk))
	for i, entry := range chunk {
		if len(entry.Surfaces) > 0 {
			var kept []string
			for _, name := range entry.Surfaces {
				if declared[name] {
					kept = append(kept, name)
				}
			}
			entry.Surfaces = kept
		}
		views[i] = entry
	}
	return out, views
}
