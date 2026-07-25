package runbook

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAuthoringSchemaProjectionKeepsActionFieldsInsideActionPayload(t *testing.T) {
	const prefix = "Portable Runbook schema projection (generated from canonical OpenSeal types): "
	raw := AuthoringSchemaProjection()
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("projection prefix = %q", raw)
	}
	var projection authoringSchemaProjection
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, prefix)), &projection); err != nil {
		t.Fatal(err)
	}
	if containsField(projection.StepFields, "resultPath") {
		t.Fatalf("Runbook Step root incorrectly advertises resultPath: %#v", projection.StepFields)
	}
	if !containsField(projection.DefinitionFields, "triggers") ||
		!containsField(projection.TriggerFields, "eventType") ||
		!containsField(projection.TriggerFields, "entrypoint") {
		t.Fatalf("Runbook trigger projection = %#v", projection)
	}
	action := projection.StepKinds[StepAction]
	if action.PayloadField != "action" ||
		!containsField(action.PayloadFields, "resultPath") ||
		!containsField(action.PayloadFields, "next") {
		t.Fatalf("action Step projection = %#v", action)
	}
	if field, ok := StepPayloadField(StepAction); !ok || field != "action" {
		t.Fatalf("action payload field = %q, %t", field, ok)
	}
	if field, ok := StepPayloadFieldForJSONField(StepAction, "resultPath"); !ok || field != "action" {
		t.Fatalf("resultPath payload field = %q, %t", field, ok)
	}
	if field, ok := StepPayloadFieldForJSONField(StepDecision, "resultPath"); ok || field != "" {
		t.Fatalf("decision resultPath payload field = %q, %t", field, ok)
	}
	if !projection.Value.ExactlyOneSource || !containsField(projection.Value.Fields, "literal") ||
		!containsField(projection.Value.Fields, "ref") || !containsField(projection.Value.Fields, "template") ||
		len(projection.Value.Forms) != 3 {
		t.Fatalf("Value projection = %#v", projection.Value)
	}
}

func containsField(fields []string, expected string) bool {
	for _, field := range fields {
		if field == expected {
			return true
		}
	}
	return false
}
