package runtime

import (
	"encoding/json"
	"testing"
)

func TestForwardPortObjectiveRunTemplatesRemovesOnlyRedundantEntrypoints(t *testing.T) {
	payload := []byte(`{
		"id":"objective:legacy",
		"cadence":{"runTemplate":{"entrypoint":"monitor","capability":{"skillId":"browser","skillVersion":"1.0.0","action":"browse"}}},
		"eventRules":{"version":"1","rules":[
			{"id":"direct","runTemplate":{"entrypoint":"handle","capability":{"skillId":"kubernetes","skillVersion":"1.0.0","action":"events"}}},
			{"id":"runbook","runTemplate":{"entrypoint":"triage"}}
		]}
	}`)

	encoded, changed, err := forwardPortObjectiveRunTemplates(payload)
	if err != nil || !changed {
		t.Fatalf("forward port changed=%t err=%v", changed, err)
	}
	var objective map[string]interface{}
	if err = json.Unmarshal(encoded, &objective); err != nil {
		t.Fatal(err)
	}
	cadence := objective["cadence"].(map[string]interface{})["runTemplate"].(map[string]interface{})
	if _, exists := cadence["entrypoint"]; exists || cadence["capability"] == nil {
		t.Fatalf("cadence runTemplate = %#v", cadence)
	}
	rules := objective["eventRules"].(map[string]interface{})["rules"].([]interface{})
	direct := rules[0].(map[string]interface{})["runTemplate"].(map[string]interface{})
	runbook := rules[1].(map[string]interface{})["runTemplate"].(map[string]interface{})
	if _, exists := direct["entrypoint"]; exists || runbook["entrypoint"] != "triage" {
		t.Fatalf("event templates direct=%#v runbook=%#v", direct, runbook)
	}

	second, changed, err := forwardPortObjectiveRunTemplates(encoded)
	if err != nil || changed || string(second) != string(encoded) {
		t.Fatalf("idempotent forward port changed=%t err=%v", changed, err)
	}
}
