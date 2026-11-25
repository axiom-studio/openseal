package models

import (
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

// A preview's blocks survive the activity event, also after the payload
// went through JSON (a database round trip).
func TestProjectTurnAnswerPreviewKeepsBlocks(t *testing.T) {
	blocks := []presentation.Component{{ID: "tip", Type: "text", Text: "Pack light."}}
	payload := map[string]interface{}{"attemptId": "a", "sequence": 2, "text": ":::text{#tip}\nPack light.\n:::", "reset": false, "blocks": blocks}
	for name, event := range map[string]map[string]interface{}{"memory": payload, "stored": roundTrip(t, payload)} {
		preview := ProjectTurnAnswerPreview(&ActivityEvent{EventType: "turn.answer_preview", Payload: event})
		if preview == nil || len(preview.Blocks) != 1 || preview.Blocks[0].ID != "tip" || preview.Blocks[0].Text != "Pack light." {
			t.Fatalf("%s: preview = %#v", name, preview)
		}
	}
}

func TestTurnAnswerPreviewResetCarriesNoBlocks(t *testing.T) {
	preview := TurnAnswerPreview{AttemptID: "a", Sequence: 1, Reset: true, Blocks: []presentation.Component{{ID: "x", Type: "text", Text: "x"}}}
	if preview.Validate() == nil {
		t.Fatal("a reset with blocks was accepted")
	}
}

func roundTrip(t *testing.T, value map[string]interface{}) map[string]interface{} {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
