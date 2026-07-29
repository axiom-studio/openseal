package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

const actionProgressIntentRole = "intent"

// computeActionProgressIntentDigest identifies the durable purpose of an
// interaction independently of volatile snapshot references and model-chosen
// idempotency keys. Skills opt in through the portable semantic argument role
// "intent"; actions without that role retain the existing exact semantic
// identity behavior.
func computeActionProgressIntentDigest(call *ActionCall, semanticArguments map[string]string) string {
	if call == nil {
		return ""
	}
	argument := strings.TrimSpace(semanticArguments[actionProgressIntentRole])
	value, ok := call.Arguments[argument].(string)
	value = strings.Join(strings.Fields(strings.ToLower(value)), " ")
	if argument == "" || !ok || value == "" {
		return ""
	}
	canonical := struct {
		DeploymentID    string `json:"deploymentId"`
		BindingID       string `json:"bindingId"`
		BindingRevision int64  `json:"bindingRevision"`
		SkillID         string `json:"skillId"`
		SkillVersion    string `json:"skillVersion"`
		Action          string `json:"action"`
		Intent          string `json:"intent"`
	}{call.DeploymentID, call.BindingID, call.BindingRevision, call.SkillID, call.SkillVersion, call.Action, value}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func annotateActionProgress(output map[string]interface{}, call *ActionCall, semanticArguments map[string]string) map[string]interface{} {
	result := deepCloneCheckpointMap(output)
	progress, ok := result["progress"].(map[string]interface{})
	if !ok {
		return result
	}
	changed, ok := progress["changed"].(bool)
	if !ok {
		return result
	}
	progress = deepCloneCheckpointMap(progress)
	delete(progress, "intentDigest")
	if digest := computeActionProgressIntentDigest(call, semanticArguments); digest != "" {
		progress["intentDigest"] = digest
	}
	progress["changed"] = changed
	result["progress"] = progress
	return result
}

func actionProgress(output interface{}) (changed bool, intentDigest, afterDigest string, ok bool) {
	result, resultOK := output.(map[string]interface{})
	if !resultOK {
		return false, "", "", false
	}
	progress, progressOK := result["progress"].(map[string]interface{})
	if !progressOK {
		return false, "", "", false
	}
	changed, ok = progress["changed"].(bool)
	intentDigest, _ = progress["intentDigest"].(string)
	afterDigest, _ = progress["afterDigest"].(string)
	return changed, intentDigest, afterDigest, ok
}

func latestObservationDigest(checkpoint map[string]interface{}) string {
	entries := actionHistoryEntries(checkpoint)
	for index := len(entries) - 1; index >= 0; index-- {
		result, _ := entries[index]["result"].(map[string]interface{})
		if value, _ := result["observationDigest"].(string); value != "" {
			return value
		}
		if _, _, value, ok := actionProgress(result); ok && value != "" {
			return value
		}
	}
	return ""
}

func matchingNoProgressAction(checkpoint map[string]interface{}, intentDigest string) map[string]interface{} {
	if intentDigest == "" {
		return nil
	}
	currentObservation := latestObservationDigest(checkpoint)
	if currentObservation == "" {
		return nil
	}
	entries := actionHistoryEntries(checkpoint)
	for index := len(entries) - 1; index >= 0; index-- {
		changed, candidateIntent, afterDigest, ok := actionProgress(entries[index]["result"])
		if ok && !changed && candidateIntent == intentDigest && afterDigest == currentObservation {
			return entries[index]
		}
	}
	return nil
}

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
		entry["result"] = compactActionResult(actionResultWithoutModelMedia(call.Output), maximumActionHistoryResultBytes)
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

// checkpointTerminalAction records the authoritative terminal ActionCall both
// in the bounded history and as the most recent result consumed by the next
// Turn. Callers may add non-secret decision metadata such as approval identity
// and disposition. Keeping this projection shared prevents approval and policy
// denials from disappearing between Turns and being proposed again.
func checkpointTerminalAction(checkpoint map[string]interface{}, call *ActionCall, metadata map[string]interface{}) map[string]interface{} {
	result := appendActionHistory(checkpoint, call)
	lastAction := map[string]interface{}{
		"actionCallId": call.ID, "bindingId": call.BindingID, "bindingRevision": call.BindingRevision,
		"skillId": call.SkillID, "skillVersion": call.SkillVersion,
		"action": call.Action, "status": call.Status,
		"arguments": deepCloneCheckpointMap(call.Arguments),
	}
	if call.ApprovalID != "" {
		lastAction["approvalId"] = call.ApprovalID
	}
	if call.Status == ActionCallStatusSucceeded {
		lastAction["result"] = boundedActionResult(actionResultWithoutModelMedia(call.Output))
		if media := actionResultModelMedia(call.Output); media != nil {
			lastAction["modelMedia"] = media
		}
	} else if call.Error != "" {
		lastAction["error"] = call.Error
	}
	for key, value := range metadata {
		lastAction[key] = deepCloneCheckpointValue(value)
	}
	result["lastAction"] = lastAction
	return result
}

func actionResultWithoutModelMedia(output map[string]interface{}) map[string]interface{} {
	result := deepCloneCheckpointMap(output)
	delete(result, "modelMedia")
	return result
}

func actionResultModelMedia(output map[string]interface{}) map[string]interface{} {
	if output == nil {
		return nil
	}
	media, ok := output["modelMedia"].(map[string]interface{})
	if !ok {
		return nil
	}
	return deepCloneCheckpointMap(media)
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
