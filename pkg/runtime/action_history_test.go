package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestActionSemanticDigestIgnoresIdempotencyAndEvidence(t *testing.T) {
	base := &ActionCall{
		DeploymentID: "agent", BindingID: "binding", BindingRevision: 2,
		SkillID: "reader", SkillVersion: "1", Action: "read",
		Arguments:      map[string]interface{}{"resource": "deployment/api"},
		IdempotencyKey: "model-key-one", EvidenceRefs: []string{"event:first"},
	}
	changed := cloneActionCall(base)
	changed.IdempotencyKey = "model-key-two"
	changed.EvidenceRefs = []string{"event:second"}
	if first, second := ComputeActionSemanticDigest(base), ComputeActionSemanticDigest(changed); first == "" || first != second {
		t.Fatalf("semantic digests differ: %q != %q", first, second)
	}
	changed.Arguments["resource"] = "deployment/worker"
	if ComputeActionSemanticDigest(base) == ComputeActionSemanticDigest(changed) {
		t.Fatal("materially different action arguments shared a semantic digest")
	}
}

func TestActionHistoryPreservesStructuredOversizedEvidenceAndBoundsEntries(t *testing.T) {
	events := make([]interface{}, 96)
	for index := range events {
		events[index] = map[string]interface{}{
			"name":    "event-" + strings.Repeat("x", 900),
			"message": "diagnostic-" + strings.Repeat("y", 900),
			"index":   index,
		}
	}
	output := map[string]interface{}{"items": events, "count": len(events)}
	compacted := compactActionResult(output, maximumActionHistoryResultBytes)
	encoded, err := json.Marshal(compacted)
	if err != nil || len(encoded) > maximumActionHistoryResultBytes {
		t.Fatalf("compacted evidence bytes=%d err=%v", len(encoded), err)
	}
	projection := compacted.(map[string]interface{})
	if projection["available"] != true || projection["truncated"] != true || projection["value"] == nil {
		t.Fatalf("oversized evidence was discarded: %#v", projection)
	}

	checkpoint := map[string]interface{}{"phase": "investigate"}
	for index := 0; index < maximumActionHistoryEntries+3; index++ {
		completed := time.Date(2026, 7, 14, 5, index, 0, 0, time.UTC)
		call := &ActionCall{
			ID: "call-" + string(rune('a'+index)), SemanticDigest: "digest-" + string(rune('a'+index)),
			SkillID: "reader", SkillVersion: "1", Action: "read", Status: ActionCallStatusSucceeded,
			Arguments: map[string]interface{}{"index": index}, Output: output, CompletedAt: &completed,
		}
		checkpoint = appendActionHistory(checkpoint, call)
	}
	entries := actionHistoryEntries(checkpoint)
	if len(entries) != maximumActionHistoryEntries || entries[0]["actionCallId"] != "call-d" || checkpoint["phase"] != "investigate" {
		t.Fatalf("bounded history = %#v", entries)
	}
	checkpointJSON, _ := json.Marshal(checkpoint)
	if strings.Contains(string(checkpointJSON), "call-a") || !strings.Contains(string(checkpointJSON), "_opensealActionHistory") {
		t.Fatalf("history retention was not bounded: %s", checkpointJSON)
	}
}

func TestPreserveKernelActionHistoryRejectsModelRewrite(t *testing.T) {
	current := map[string]interface{}{actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"actionCallId": "trusted"}}, "phase": "old"}
	proposed := map[string]interface{}{actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"actionCallId": "invented"}}, "phase": "new"}
	merged := preserveKernelActionHistory(current, proposed)
	if merged["phase"] != "new" || actionHistoryEntries(merged)[0]["actionCallId"] != "trusted" {
		t.Fatalf("merged checkpoint = %#v", merged)
	}
	withoutHistory := preserveKernelActionHistory(nil, proposed)
	if len(actionHistoryEntries(withoutHistory)) != 0 {
		t.Fatalf("model invented kernel action history: %#v", withoutHistory)
	}
}

func TestTerminalActionSeparatesModelMediaFromTextEvidence(t *testing.T) {
	call := &ActionCall{
		ID: "screenshot-call", SkillID: "browser", SkillVersion: "1", Action: "snapshot",
		Status: ActionCallStatusSucceeded,
		Output: map[string]interface{}{
			"generation": 3,
			"modelMedia": map[string]interface{}{
				"mediaType": "image/jpeg", "contentBase64": "c2NyZWVuc2hvdA==", "detail": "low",
			},
		},
	}
	checkpoint := checkpointTerminalAction(nil, call, nil)
	last := checkpoint["lastAction"].(map[string]interface{})
	if last["modelMedia"].(map[string]interface{})["contentBase64"] != "c2NyZWVuc2hvdA==" {
		t.Fatalf("model media was not preserved separately: %#v", last)
	}
	if _, leaked := last["result"].(map[string]interface{})["modelMedia"]; leaked {
		t.Fatalf("model media leaked into textual action result: %#v", last["result"])
	}
	entries := actionHistoryEntries(checkpoint)
	if _, leaked := entries[0]["result"].(map[string]interface{})["modelMedia"]; leaked {
		t.Fatalf("model media leaked into durable history: %#v", entries[0])
	}
}
