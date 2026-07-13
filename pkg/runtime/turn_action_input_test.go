package runtime

import "testing"

func TestResolveTurnActionInputUsesBoundedJSONPointer(t *testing.T) {
	checkpoint := map[string]interface{}{"inputs": map[string]interface{}{
		"release/one": map[string]interface{}{"environment": "staging"},
	}}
	arguments, err := resolveTurnActionInput(checkpoint, "#/inputs/release~1one")
	if err != nil {
		t.Fatal(err)
	}
	if arguments["environment"] != "staging" {
		t.Fatalf("arguments = %#v", arguments)
	}
	arguments["environment"] = "mutated"
	if checkpoint["inputs"].(map[string]interface{})["release/one"].(map[string]interface{})["environment"] != "staging" {
		t.Fatal("resolved action input aliases the durable checkpoint")
	}
}

func TestResolveTurnActionInputAcceptsExplicitCheckpointRoot(t *testing.T) {
	checkpoint := map[string]interface{}{"actionInputs": map[string]interface{}{
		"sendEmail": map[string]interface{}{"subject": "Reviewed report"},
	}}
	for _, reference := range []string{
		"/continuationCheckpoint/actionInputs/sendEmail",
		"#/continuationCheckpoint/actionInputs/sendEmail",
	} {
		arguments, err := resolveTurnActionInput(checkpoint, reference)
		if err != nil || arguments["subject"] != "Reviewed report" {
			t.Fatalf("reference %q arguments=%#v err=%v", reference, arguments, err)
		}
	}
	if _, err := resolveTurnActionInput(checkpoint, "/continuationCheckpoint"); err == nil {
		t.Fatal("checkpoint root was accepted as action input")
	}
}

func TestResolveTurnActionInputRejectsMissingAndScalarReferences(t *testing.T) {
	checkpoint := map[string]interface{}{"inputs": map[string]interface{}{"value": "scalar"}}
	for _, reference := range []string{"inputs/value", "/inputs/missing", "/inputs/value"} {
		if _, err := resolveTurnActionInput(checkpoint, reference); err == nil {
			t.Fatalf("invalid reference %q was accepted", reference)
		}
	}
}
