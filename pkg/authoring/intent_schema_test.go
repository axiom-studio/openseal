package authoring

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
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
		`"activation"`,
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

func TestAuthoringProviderRequestProjectsExistingStateSemantically(t *testing.T) {
	existing := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "tenant/example/analyst", Version: "1.0.0", DisplayName: "Analyst", Purpose: "Analyze evidence", SystemPrompt: "Analyze evidence accurately.",
		Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	}}}
	projected := promptGenerateRequest(GenerateRequest{Mode: ModeAmend, Prompt: "Rename it", Existing: &existing, Form: AuthoringForm{
		Version: AuthoringFormVersionV1,
		Fields:  []AuthoringFormField{{ID: "compiler-owned", CompilerOutput: "candidate.agents[].authority"}},
	}})
	payload, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, forbidden := range []string{"authoringForm", "compilerOutput", "candidate", "systemPrompt", "authority", "runbook", "resultPath"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("provider request leaks compiler-owned field %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"behavior":"Analyze evidence accurately."`) {
		t.Fatalf("semantic behavior was not projected: %s", text)
	}
}
