package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestHostedCheckpointDeduplicatesOnlyIdenticalCurrentArguments(t *testing.T) {
	large := strings.Repeat("Exact document content with corrections. ", 500)
	for _, tc := range []struct {
		name      string
		lastID    interface{}
		historyID interface{}
		current   interface{}
		previous  interface{}
		wantRef   bool
	}{
		{"large identical", "call", "call", map[string]interface{}{"body": large}, map[string]interface{}{"body": large}, true},
		{"nested identical", "call", "call", map[string]interface{}{"pages": []interface{}{large}}, map[string]interface{}{"pages": []interface{}{large}}, true},
		{"different arguments", "call", "call", map[string]interface{}{"body": large}, map[string]interface{}{"body": large + " corrected"}, false},
		{"different action", "new", "old", map[string]interface{}{"body": large}, map[string]interface{}{"body": large}, false},
		{"missing identity", nil, nil, map[string]interface{}{"body": large}, map[string]interface{}{"body": large}, false},
		{"empty identity", "", "", map[string]interface{}{"body": large}, map[string]interface{}{"body": large}, false},
		{"small arguments", "call", "call", map[string]interface{}{"limit": 2}, map[string]interface{}{"limit": 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := map[string]interface{}{
				"lastAction":               map[string]interface{}{"actionCallId": tc.lastID, "arguments": tc.current, "status": "succeeded"},
				actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"actionCallId": tc.historyID, "arguments": tc.previous, "status": "succeeded", "semanticDigest": "unchanged"}},
			}
			before, _ := json.Marshal(checkpoint)
			input, err := MarshalHostedTurnModelInput(HostedTurnRequest{Goal: "Continue", ContinuationCheckpoint: checkpoint})
			if err != nil {
				t.Fatal(err)
			}
			var projected HostedTurnModelInput
			if err := json.Unmarshal(input, &projected); err != nil {
				t.Fatal(err)
			}
			entry := projected.ContinuationCheckpoint[actionHistoryCheckpointKey].([]interface{})[0].(map[string]interface{})
			_, referenced := entry["argumentsRef"]
			if referenced != tc.wantRef {
				t.Fatalf("reference present=%v, want %v", referenced, tc.wantRef)
			}
			if referenced {
				if entry["argumentsRef"] != "continuationCheckpoint.lastAction.arguments" || entry["arguments"] != nil {
					t.Fatal("invalid argument reference")
				}
				if len(input) >= len(before)-len(large)+256 {
					t.Fatal("duplicate large arguments were not removed")
				}
			} else {
				// Non-duplicate arguments stay, with only long strings elided.
				got, _ := json.Marshal(entry["arguments"])
				want, _ := json.Marshal(elideHostedTurnArgumentValue(deepCloneCheckpointValue(tc.previous)))
				if !bytes.Equal(got, want) {
					t.Fatal("changed non-duplicate arguments")
				}
			}
			last := projected.ContinuationCheckpoint["lastAction"].(map[string]interface{})
			got, _ := json.Marshal(last["arguments"])
			want, _ := json.Marshal(elideHostedTurnArgumentValue(deepCloneCheckpointValue(tc.current)))
			if !bytes.Equal(got, want) || entry["semanticDigest"] != "unchanged" {
				t.Fatal("lost exact input or evidence identity")
			}
			after, _ := json.Marshal(checkpoint)
			if !bytes.Equal(before, after) {
				t.Fatal("mutated durable checkpoint")
			}
			if tc.wantRef {
				t.Logf("checkpoint bytes before=%d; complete projected input bytes after=%d", len(before), len(input))
			}
		})
	}
}

func TestHostedCheckpointDoesNotInventResultReferenceWithoutActionIdentity(t *testing.T) {
	checkpoint := map[string]interface{}{"lastAction": map[string]interface{}{"result": map[string]interface{}{"text": "latest"}},
		actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"result": map[string]interface{}{"text": "older"}}}}
	input, err := MarshalHostedTurnModelInput(HostedTurnRequest{ContinuationCheckpoint: checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(input, []byte("currentResultRef")) {
		t.Fatal("aliased unrelated results without an action identity")
	}
	if !bytes.Contains(input, []byte("older")) {
		t.Fatal("lost historical result")
	}
}

func elisionCheckpoint(lastStatus ActionCallStatus, lastContent string) map[string]interface{} {
	large := strings.Repeat("a", 4096)
	return map[string]interface{}{
		"actionInputs":                map[string]interface{}{"proposed": map[string]interface{}{"write": []interface{}{map[string]interface{}{"path": "src/App.tsx", "content": large}}}},
		proposalRecoveryCheckpointKey: map[string]interface{}{"error": "rejected", "arguments": map[string]interface{}{"content": large}},
		actionHistoryCheckpointKey: []interface{}{
			map[string]interface{}{
				"actionCallId": "call-1", "action": "update_app", "status": "succeeded",
				"arguments": map[string]interface{}{
					"appId": "app-1", "note": "short",
					"write": []interface{}{map[string]interface{}{"path": "src/App.tsx", "content": large}},
					"files": []interface{}{map[string]interface{}{"path": "src/a.ts", "content": "tiny"}},
				},
				"result": map[string]interface{}{"status": "saved"},
			},
			map[string]interface{}{
				"actionCallId": "call-2", "action": "update_app", "status": string(lastStatus),
				"arguments": map[string]interface{}{"appId": "app-1", "replace": []interface{}{map[string]interface{}{"path": "src/App.tsx", "find": "x", "replace": lastContent}}},
			},
		},
		"lastAction": map[string]interface{}{
			"actionCallId": "call-2", "action": "update_app", "status": string(lastStatus),
			"arguments": map[string]interface{}{"appId": "app-1", "replace": []interface{}{map[string]interface{}{"path": "src/App.tsx", "find": "x", "replace": lastContent}}},
		},
	}
}

func TestHostedModelCheckpointElidesLargeHistoricalArguments(t *testing.T) {
	large := strings.Repeat("a", 4096)
	durable := elisionCheckpoint(ActionCallStatusSucceeded, strings.Repeat("b", 2048))
	view := hostedTurnModelCheckpoint(durable)

	first := view[actionHistoryCheckpointKey].([]interface{})[0].(map[string]interface{})["arguments"].(map[string]interface{})
	written := first["write"].([]interface{})[0].(map[string]interface{})
	digest := sha256.Sum256([]byte(large))
	elided, ok := written["content"].(map[string]interface{})
	if written["path"] != "src/App.tsx" || !ok || elided["elided"] != true || elided["bytes"] != 4096 || elided["sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatalf("expected elided content with path kept, got %#v", written)
	}
	if first["appId"] != "app-1" || first["note"] != "short" || first["files"].([]interface{})[0].(map[string]interface{})["content"] != "tiny" {
		t.Fatalf("short arguments must stay verbatim: %#v", first)
	}
	last := view["lastAction"].(map[string]interface{})["arguments"].(map[string]interface{})
	if replaced, ok := last["replace"].([]interface{})[0].(map[string]interface{})["replace"].(map[string]interface{}); !ok || replaced["bytes"] != 2048 {
		t.Fatalf("succeeded lastAction arguments should be elided: %#v", last)
	}
	// The history entry for the last action points at lastAction instead.
	if ref := view[actionHistoryCheckpointKey].([]interface{})[1].(map[string]interface{})["argumentsRef"]; ref != "continuationCheckpoint.lastAction.arguments" {
		t.Fatalf("expected argumentsRef dedupe, got %v", ref)
	}
	if view["actionInputs"].(map[string]interface{})["proposed"].(map[string]interface{})["write"].([]interface{})[0].(map[string]interface{})["content"] != large {
		t.Fatal("actionInputs must not be elided")
	}
	if view[proposalRecoveryCheckpointKey].(map[string]interface{})["arguments"].(map[string]interface{})["content"] != large {
		t.Fatal("proposal recovery must not be elided")
	}
}

func TestHostedModelCheckpointKeepsSmallFailedLastActionArguments(t *testing.T) {
	failedContent := strings.Repeat("c", 8192)
	view := hostedTurnModelCheckpoint(elisionCheckpoint(ActionCallStatusFailed, failedContent))
	last := view["lastAction"].(map[string]interface{})["arguments"].(map[string]interface{})
	if last["replace"].([]interface{})[0].(map[string]interface{})["replace"] != failedContent {
		t.Fatalf("failed lastAction arguments under 16 KB must stay whole: %#v", last)
	}

	view = hostedTurnModelCheckpoint(elisionCheckpoint(ActionCallStatusFailed, strings.Repeat("c", 20<<10)))
	last = view["lastAction"].(map[string]interface{})["arguments"].(map[string]interface{})
	if _, ok := last["replace"].([]interface{})[0].(map[string]interface{})["replace"].(map[string]interface{}); !ok {
		t.Fatalf("failed lastAction arguments over 16 KB should be elided: %#v", last)
	}
}

func TestHostedModelInputLeavesDurableCheckpointUnchanged(t *testing.T) {
	durable := elisionCheckpoint(ActionCallStatusSucceeded, strings.Repeat("b", 2048))
	durable["lastAction"].(map[string]interface{})["modelMedia"] = map[string]interface{}{"mediaType": "image/png", "detail": "high", "data": "secret"}
	before := deepCloneCheckpointMap(durable)
	encodedBefore, _ := json.Marshal(durable)

	input, err := MarshalHostedTurnModelInput(HostedTurnRequest{Goal: "edit the app", ContinuationCheckpoint: durable})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, durable) {
		t.Fatal("building the model input changed the durable checkpoint")
	}
	if encodedAfter, _ := json.Marshal(durable); string(encodedAfter) != string(encodedBefore) {
		t.Fatal("durable checkpoint encoding changed")
	}
	if !strings.Contains(string(input), `"elided":true`) || strings.Contains(string(input), strings.Repeat("b", 2048)) {
		t.Fatal("model input should carry the elided view")
	}
}
