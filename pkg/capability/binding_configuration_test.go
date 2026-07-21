package capability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBindingConfigurationFieldsAreTypedBoundedAndSecretFree(t *testing.T) {
	clusterID := int64(7)
	fields := []BindingConfigurationFieldChoice{{
		CatalogSkillID: "openseal.kubernetes",
		Skill:          NewSkillIdentity("openseal.kubernetes", "1.1.0", "builtin:openseal.kubernetes"),
		Key:            "clusterId", Type: "integer", Required: true, Prompt: "Which Kubernetes cluster should this Agent operate?",
		Options: []BindingConfigurationOption{{Label: "Development", Description: "Tenant development cluster", Value: BindingConfigurationValue{Integer: &clusterID}}},
	}}
	if err := ValidateBindingConfigurationFields(fields); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"integer":7`) || strings.Contains(strings.ToLower(string(encoded)), "kubeconfig") {
		t.Fatalf("binding configuration fields = %s", encoded)
	}

	invalid := fields
	invalid[0].Key = "api_token"
	if err := ValidateBindingConfigurationFields(invalid); err == nil || !strings.Contains(err.Error(), "secret-like") {
		t.Fatalf("secret-like field accepted: %v", err)
	}
	fields[0].Key = "clusterId"
	invalid = fields
	invalid[0].Options = append(invalid[0].Options, invalid[0].Options[0])
	if err := ValidateBindingConfigurationFields(invalid); err == nil || !strings.Contains(err.Error(), "duplicate option value") {
		t.Fatalf("duplicate option accepted: %v", err)
	}
	wrong := "7"
	invalid = fields
	invalid[0].Options = []BindingConfigurationOption{{Label: "Development", Value: BindingConfigurationValue{String: &wrong}}}
	if err := ValidateBindingConfigurationFields(invalid); err == nil || !strings.Contains(err.Error(), "does not match type") {
		t.Fatalf("wrong typed option accepted: %v", err)
	}
}
