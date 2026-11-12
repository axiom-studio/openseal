package routine

import (
	"encoding/json"
	"maps"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// runbookActionResultLimit caps the action output an action node writes into
// the engine state. That state is written on every step and copied into every
// fork branch, so it never carries media or unbounded payloads.
const runbookActionResultLimit = 64 << 10

// runbookResultCompactLevel bounds the depth, entries and text of a
// compacted action result.
type runbookResultCompactLevel struct{ depth, entries, text int }

// runbookActionResult is the value an action node writes to its result path:
// the ActionCall output without its model media (screenshots and other
// base64 payloads), compacted to runbookActionResultLimit when it is larger.
func runbookActionResult(output map[string]interface{}) map[string]interface{} {
	result := maps.Clone(output)
	delete(result, "modelMedia")
	if len(result) == 0 {
		return map[string]interface{}{}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return map[string]interface{}{
			"available": false, "reason": "action result is not JSON: " + err.Error(),
		}
	}
	if len(encoded) <= runbookActionResultLimit {
		// The round trip is a deep copy: the engine state shares nothing
		// with the stored ActionCall.
		var copied map[string]interface{}
		if json.Unmarshal(encoded, &copied) == nil {
			return copied
		}
	}
	original := len(encoded)
	for _, level := range []runbookResultCompactLevel{
		{8, 64, 2048}, {6, 16, 512}, {4, 4, 128},
	} {
		value := map[string]interface{}{
			"available": true, "truncated": true, "originalSizeBytes": original,
			"value": runbookCompactValue(result, level.depth, level.entries, level.text),
		}
		if candidate, err := json.Marshal(value); err == nil && len(candidate) <= runbookActionResultLimit {
			return value
		}
	}
	return map[string]interface{}{
		"available": false, "truncated": true, "originalSizeBytes": original,
		"reason": "action result is larger than the runbook state allows",
	}
}

// runbookCompactValue samples a value to bounded depth, entry count and text
// length, keeping the head and tail of long arrays.
func runbookCompactValue(value interface{}, depth, entries, text int) interface{} {
	if depth <= 0 {
		return map[string]interface{}{"_opensealTruncated": "maximum depth reached"}
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]interface{}, min(len(keys), entries)+1)
		for _, key := range keys[:min(len(keys), entries)] {
			result[key] = runbookCompactValue(typed[key], depth-1, entries, text)
		}
		if len(keys) > entries {
			result["_opensealOmittedFields"] = len(keys) - entries
		}
		return result
	case []interface{}:
		if len(typed) <= entries {
			result := make([]interface{}, len(typed))
			for index := range typed {
				result[index] = runbookCompactValue(typed[index], depth-1, entries, text)
			}
			return result
		}
		head := (entries + 1) / 2
		tail := entries - head
		result := make([]interface{}, 0, entries+1)
		for index := 0; index < head; index++ {
			result = append(result, runbookCompactValue(typed[index], depth-1, entries, text))
		}
		result = append(result, map[string]interface{}{"_opensealOmittedItems": len(typed) - entries})
		for index := len(typed) - tail; index < len(typed); index++ {
			result = append(result, runbookCompactValue(typed[index], depth-1, entries, text))
		}
		return result
	case string:
		if len(typed) <= text {
			return typed
		}
		return typed[:text] + "…"
	default:
		return typed
	}
}

// runbookToolResultText is the text of a terminal tool result, for failure
// messages.
func runbookToolResultText(result *modelstep.LoopToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	return strings.TrimSpace(control.ResultText(&models.ToolResult{Content: result.Content}))
}
