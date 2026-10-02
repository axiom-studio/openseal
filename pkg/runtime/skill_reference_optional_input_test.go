package runtime

import (
	"encoding/json"
	"testing"
)

func TestCompatibleUpgradeInputSchema(t *testing.T) {
	decode := func(s string) map[string]interface{} {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	base := `{"type":"object","additionalProperties":false,"properties":{"url":{"type":"string"}},"required":["url"]}`
	for _, tt := range []struct {
		name, next string
		compatible bool
	}{
		{"unchanged", base, true},
		{"optional input", `{"type":"object","additionalProperties":false,"properties":{"url":{"type":"string"},"displayName":{"type":"string"}},"required":["url"]}`, true},
		{"new required input", `{"type":"object","additionalProperties":false,"properties":{"url":{"type":"string"},"displayName":{"type":"string"}},"required":["url","displayName"]}`, false},
		{"changed existing input", `{"type":"object","additionalProperties":false,"properties":{"url":{"type":"number"}},"required":["url"]}`, false},
		{"removed input", `{"type":"object","additionalProperties":false,"properties":{},"required":["url"]}`, false},
		{"new cross-field constraint", `{"type":"object","additionalProperties":false,"properties":{"url":{"type":"string"}},"required":["url"],"allOf":[]}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := compatibleUpgradeInputSchema(decode(base), decode(tt.next)); got != tt.compatible {
				t.Fatalf("compatible=%v", got)
			}
		})
	}
	open := decode(base)
	open["additionalProperties"] = true
	next := decode(base)
	next["additionalProperties"] = true
	next["properties"].(map[string]interface{})["name"] = map[string]interface{}{"type": "string"}
	if compatibleUpgradeInputSchema(open, next) {
		t.Fatal("restricted previously open property")
	}
}
