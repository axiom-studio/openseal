package runtime

import (
	"encoding/json"
	"fmt"
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

func TestActionProgressIntentIgnoresRegeneratedReferencesAndKeys(t *testing.T) {
	base := &ActionCall{
		DeploymentID: "agent", BindingID: "browser", BindingRevision: 2,
		SkillID: "skill-browser", SkillVersion: "2", Action: "camoufox-click",
		Arguments: map[string]interface{}{
			"sessionId": "run-1", "target": "s1:e26", "intent": " Open   the Community Rules ", "idempotencyKey": "first-key",
		},
	}
	changed := cloneActionCall(base)
	changed.Arguments["target"] = "s9:e47"
	changed.Arguments["idempotencyKey"] = "second-key"
	semantic := map[string]string{"intent": "intent"}
	first, second := computeActionProgressIntentDigest(base, semantic), computeActionProgressIntentDigest(changed, semantic)
	if first == "" || first != second {
		t.Fatalf("progress intent digests differ: %q != %q", first, second)
	}
	changed.Arguments["intent"] = "Open account settings"
	if computeActionProgressIntentDigest(base, semantic) == computeActionProgressIntentDigest(changed, semantic) {
		t.Fatal("different purposes shared a progress intent digest")
	}
}

func TestMatchingNoProgressActionRequiresSameCurrentObservation(t *testing.T) {
	call := &ActionCall{DeploymentID: "agent", BindingID: "browser", BindingRevision: 1, SkillID: "skill-browser", SkillVersion: "2", Action: "camoufox-click", Arguments: map[string]interface{}{"intent": "Open rules"}}
	intent := computeActionProgressIntentDigest(call, map[string]string{"intent": "intent"})
	call.ID, call.Status = "stagnant", ActionCallStatusSucceeded
	call.Output = map[string]interface{}{"progress": map[string]interface{}{"changed": false, "intentDigest": intent, "afterDigest": "sha256:same"}}
	checkpoint := appendActionHistory(nil, call)
	checkpoint = appendActionHistory(checkpoint, &ActionCall{ID: "snapshot-same", Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"observationDigest": "sha256:same"}})
	if matchingNoProgressAction(checkpoint, intent) == nil {
		t.Fatal("regenerated observation did not retain no-progress backpressure")
	}
	checkpoint = appendActionHistory(checkpoint, &ActionCall{ID: "snapshot-changed", Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"observationDigest": "sha256:changed"}})
	if matchingNoProgressAction(checkpoint, intent) != nil {
		t.Fatal("stale no-progress evidence blocked an action after state changed")
	}
}

func TestAnnotateActionProgressUsesKernelComputedIntent(t *testing.T) {
	call := &ActionCall{
		DeploymentID: "agent", BindingID: "browser", BindingRevision: 1,
		SkillID: "skill-browser", SkillVersion: "2", Action: "camoufox-click",
		Arguments: map[string]interface{}{"intent": "Open rules"},
	}
	output := map[string]interface{}{"progress": map[string]interface{}{
		"changed": false, "beforeDigest": "sha256:same", "afterDigest": "sha256:same", "intentDigest": "untrusted",
	}}
	annotated := annotateActionProgress(output, call, map[string]string{"intent": "intent"})
	progress := annotated["progress"].(map[string]interface{})
	if progress["intentDigest"] == "" || progress["intentDigest"] == "untrusted" {
		t.Fatalf("kernel intent digest was not authoritative: %#v", progress)
	}
	if output["progress"].(map[string]interface{})["intentDigest"] != "untrusted" {
		t.Fatal("dispatcher output was mutated in place")
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

func TestCompactActionResultPreservesMiddleFormControls(t *testing.T) {
	elements := make([]interface{}, 180)
	for index := range elements {
		elements[index] = map[string]interface{}{
			"ref": fmt.Sprintf("s4:e%d", index+1), "role": "link", "name": fmt.Sprintf("ordinary link %d", index+1),
		}
	}
	elements[57] = map[string]interface{}{
		"ref": "s4:e58", "role": "textbox", "name": "comment", "state": map[string]interface{}{"filled": false},
	}
	elements[58] = map[string]interface{}{
		"ref": "s4:e59", "role": "button", "name": "save", "state": map[string]interface{}{"type": "submit"},
	}
	compacted := compactActionResult(map[string]interface{}{
		"url": "https://example.test/thread", "elements": elements, "text": strings.Repeat("thread context ", 5000),
	}, maximumActionHistoryResultBytes)
	encoded, err := json.Marshal(compacted)
	if err != nil || len(encoded) > maximumActionHistoryResultBytes {
		t.Fatalf("compacted observation bytes=%d err=%v", len(encoded), err)
	}
	if !strings.Contains(string(encoded), `"ref":"s4:e58"`) || !strings.Contains(string(encoded), `"ref":"s4:e59"`) {
		t.Fatalf("middle form controls were omitted: %s", encoded)
	}
}

func TestPreserveKernelActionHistoryRejectsModelRewrite(t *testing.T) {
	current := map[string]interface{}{
		actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"actionCallId": "trusted"}},
		"lastAction": map[string]interface{}{
			"actionCallId": "trusted", "status": ActionCallStatusSucceeded,
			"result": map[string]interface{}{"generation": 6, "elements": []interface{}{map[string]interface{}{"ref": "s6:e57"}}},
		},
		"phase": "old",
	}
	proposed := map[string]interface{}{
		actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"actionCallId": "invented"}},
		"lastAction": map[string]interface{}{
			"actionCallId": "trusted", "status": ActionCallStatusSucceeded,
			"result": map[string]interface{}{"compacted": true, "generation": 6},
		},
		"phase": "new",
	}
	merged := preserveKernelActionHistory(current, proposed)
	if merged["phase"] != "new" || actionHistoryEntries(merged)[0]["actionCallId"] != "trusted" {
		t.Fatalf("merged checkpoint = %#v", merged)
	}
	last := merged["lastAction"].(map[string]interface{})
	elements := last["result"].(map[string]interface{})["elements"].([]interface{})
	if last["actionCallId"] != "trusted" || elements[0].(map[string]interface{})["ref"] != "s6:e57" {
		t.Fatalf("kernel-owned latest action was replaced: %#v", last)
	}
	if _, err := currentObservationElement(&AgentRun{Checkpoint: merged}, "s6:e57"); err != nil {
		t.Fatalf("current observation reference was lost across hosted turn: %v", err)
	}
	withoutHistory := preserveKernelActionHistory(nil, proposed)
	if len(actionHistoryEntries(withoutHistory)) != 0 {
		t.Fatalf("model invented kernel action history: %#v", withoutHistory)
	}
	if _, invented := withoutHistory["lastAction"]; invented {
		t.Fatalf("model invented kernel latest action: %#v", withoutHistory)
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
