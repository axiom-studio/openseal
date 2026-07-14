package runtime

import (
	"encoding/json"
	"sort"
	"time"
)

const (
	actionHistoryCheckpointKey         = "_opensealActionHistory"
	maximumActionHistoryEntries        = 16
	maximumActionHistoryResultBytes    = 16 << 10
	maximumCheckpointActionResultBytes = 64 << 10
)

// preserveKernelActionHistory makes the action evidence journal kernel-owned:
// a hosted model may read it, but omitting or rewriting it cannot erase the
// authoritative evidence accumulated by earlier action workers.
func preserveKernelActionHistory(current, proposed map[string]interface{}) map[string]interface{} {
	result := deepCloneCheckpointMap(proposed)
	if result == nil {
		result = make(map[string]interface{})
	}
	delete(result, actionHistoryCheckpointKey)
	if current != nil {
		if history, ok := current[actionHistoryCheckpointKey]; ok {
			result[actionHistoryCheckpointKey] = deepCloneCheckpointValue(history)
		}
	}
	return result
}

func appendActionHistory(checkpoint map[string]interface{}, call *ActionCall) map[string]interface{} {
	result := deepCloneCheckpointMap(checkpoint)
	if result == nil {
		result = make(map[string]interface{})
	}
	entries := actionHistoryEntries(result)
	semanticDigest := call.SemanticDigest
	if semanticDigest == "" {
		semanticDigest = ComputeActionSemanticDigest(call)
	}
	entry := map[string]interface{}{
		"actionCallId": call.ID, "semanticDigest": semanticDigest,
		"bindingId": call.BindingID, "bindingRevision": call.BindingRevision,
		"skillId": call.SkillID, "skillVersion": call.SkillVersion,
		"action": call.Action, "arguments": deepCloneCheckpointMap(call.Arguments),
		"status": call.Status,
	}
	if call.CompletedAt != nil {
		entry["completedAt"] = call.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	if call.Status == ActionCallStatusSucceeded {
		entry["result"] = compactActionResult(call.Output, maximumActionHistoryResultBytes)
	} else if call.Error != "" {
		entry["error"] = call.Error
	}
	entries = append(entries, entry)
	if len(entries) > maximumActionHistoryEntries {
		entries = entries[len(entries)-maximumActionHistoryEntries:]
	}
	values := make([]interface{}, len(entries))
	for index := range entries {
		values[index] = entries[index]
	}
	result[actionHistoryCheckpointKey] = values
	return result
}

func actionHistoryEntries(checkpoint map[string]interface{}) []map[string]interface{} {
	if checkpoint == nil {
		return nil
	}
	raw, ok := checkpoint[actionHistoryCheckpointKey]
	if !ok {
		return nil
	}
	var values []interface{}
	switch typed := raw.(type) {
	case []interface{}:
		values = typed
	case []map[string]interface{}:
		values = make([]interface{}, len(typed))
		for index := range typed {
			values[index] = typed[index]
		}
	default:
		return nil
	}
	result := make([]map[string]interface{}, 0, len(values))
	for _, value := range values {
		entry, ok := value.(map[string]interface{})
		if ok {
			result = append(result, deepCloneCheckpointMap(entry))
		}
	}
	return result
}

func succeededActionHistoryEntry(checkpoint map[string]interface{}, semanticDigest string) map[string]interface{} {
	entries := actionHistoryEntries(checkpoint)
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index]["semanticDigest"] == semanticDigest && entries[index]["status"] == string(ActionCallStatusSucceeded) {
			return entries[index]
		}
		// In-memory checkpoints retain the named string type until a store
		// round-trip; accept it without weakening the persisted comparison.
		if entries[index]["semanticDigest"] == semanticDigest && entries[index]["status"] == ActionCallStatusSucceeded {
			return entries[index]
		}
	}
	return nil
}

func boundedActionResult(output map[string]interface{}) interface{} {
	return compactActionResult(output, maximumCheckpointActionResultBytes)
}

// compactActionResult preserves structured representative evidence instead of
// replacing an oversized result with an unavailable marker. Successive,
// deterministic limits retain both the beginning and end of large arrays so a
// recent event is not silently hidden merely because older evidence is large.
func compactActionResult(output map[string]interface{}, maximumBytes int) interface{} {
	if output == nil {
		return map[string]interface{}{}
	}
	encoded, err := json.Marshal(output)
	if err == nil && len(encoded) <= maximumBytes {
		return deepCloneCheckpointMap(output)
	}
	originalSize := len(encoded)
	levels := []struct{ depth, entries, text int }{
		{8, 64, 2048}, {7, 32, 1024}, {6, 16, 512}, {5, 8, 256}, {4, 4, 128},
	}
	for _, level := range levels {
		value := map[string]interface{}{
			"available": true, "truncated": true, "originalSizeBytes": originalSize,
			"value": compactCheckpointValue(output, level.depth, level.entries, level.text),
		}
		candidate, marshalErr := json.Marshal(value)
		if marshalErr == nil && len(candidate) <= maximumBytes {
			return value
		}
	}
	return map[string]interface{}{
		"available": false, "truncated": true, "originalSizeBytes": originalSize,
		"reason": "result could not be represented within the model evidence limit",
	}
}

func compactCheckpointValue(value interface{}, depth, entries, text int) interface{} {
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
			result[key] = compactCheckpointValue(typed[key], depth-1, entries, text)
		}
		if len(keys) > entries {
			result["_opensealOmittedFields"] = len(keys) - entries
		}
		return result
	case []interface{}:
		if len(typed) <= entries {
			result := make([]interface{}, len(typed))
			for index := range typed {
				result[index] = compactCheckpointValue(typed[index], depth-1, entries, text)
			}
			return result
		}
		head := (entries + 1) / 2
		tail := entries - head
		result := make([]interface{}, 0, entries+1)
		for index := 0; index < head; index++ {
			result = append(result, compactCheckpointValue(typed[index], depth-1, entries, text))
		}
		result = append(result, map[string]interface{}{"_opensealOmittedItems": len(typed) - entries})
		for index := len(typed) - tail; index < len(typed); index++ {
			result = append(result, compactCheckpointValue(typed[index], depth-1, entries, text))
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

func deepCloneCheckpointMap(value map[string]interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	result := make(map[string]interface{}, len(value))
	for key, child := range value {
		result[key] = deepCloneCheckpointValue(child)
	}
	return result
}

func deepCloneCheckpointValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return deepCloneCheckpointMap(typed)
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index := range typed {
			result[index] = deepCloneCheckpointValue(typed[index])
		}
		return result
	case []map[string]interface{}:
		result := make([]interface{}, len(typed))
		for index := range typed {
			result[index] = deepCloneCheckpointMap(typed[index])
		}
		return result
	default:
		return typed
	}
}
