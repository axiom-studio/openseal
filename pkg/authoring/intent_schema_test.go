package authoring

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAuthoringIntentSchemaExcludesRuntimeDomainContracts(t *testing.T) {
	schema, err := AuthoringIntentJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, forbidden := range []string{
		"WorkforceCandidate", "AgentDefinition", "runbook_Definition", "runbook_Step",
		"resultPath", "entrypoints", "maxTotalTokens", "standingGrants", "approvalDestinations",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("provider form exposes compiler-owned contract %q", forbidden)
		}
	}
	properties := schema["properties"].(map[string]interface{})
	version := properties["schemaVersion"].(map[string]interface{})
	if version["const"] != AuthoringIntentSchemaVersion {
		t.Fatalf("schema version = %#v", version)
	}
}

func TestAuthoringIntentSchemaConstrainsSemanticVocabularies(t *testing.T) {
	schema, err := AuthoringIntentJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(schema)
	text := string(payload)
	for _, expected := range []string{`"agent"`, `"team"`, `"workforce"`, `"on_demand"`, `"schedule"`, `"event"`, `"by_policy"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("semantic schema is missing %s", expected)
		}
	}
}
