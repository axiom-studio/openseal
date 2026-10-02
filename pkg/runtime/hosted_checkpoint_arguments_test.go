package runtime

import (
	"bytes"
	"encoding/json"
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
				got, _ := json.Marshal(entry["arguments"])
				want, _ := json.Marshal(tc.previous)
				if !bytes.Equal(got, want) {
					t.Fatal("changed non-duplicate arguments")
				}
			}
			last := projected.ContinuationCheckpoint["lastAction"].(map[string]interface{})
			got, _ := json.Marshal(last["arguments"])
			want, _ := json.Marshal(tc.current)
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
