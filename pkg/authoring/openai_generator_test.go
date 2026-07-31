package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestOpenAICompatibleGeneratorUsesStrictJSONTransportWithoutLeakingKey(t *testing.T) {
	apiKey := "transport-secret"
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+apiKey {
			t.Fatalf("request method/auth = %s / %q", request.Method, request.Header.Get("Authorization"))
		}
		if request.Header.Get("Idempotency-Key") != "change-set:one:0" {
			t.Fatalf("idempotency header = %q", request.Header.Get("Idempotency-Key"))
		}
		var body map[string]interface{}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body)
		requestBody = string(encoded)
		if body["model"] != "deepseek-v4-flash" || body["response_format"].(map[string]interface{})["type"] != "json_object" {
			t.Fatalf("request body = %#v", body)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("default transport emitted optional sampling parameter: %#v", body["temperature"])
		}
		if _, exists := body["thinking"]; exists {
			t.Fatalf("default transport emitted provider-specific thinking field: %#v", body["thinking"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"candidate\":{\"agents\":[],\"assignments\":[]},\"questions\":[\"Which Team should be created?\"]}"}}]}`))
	}))
	defer server.Close()
	generator, err := NewOpenAICompatibleGeneratorWithOptions(server.URL, apiKey, "deepseek-v4-flash", server.Client(), OpenAICompatibleGeneratorOptions{StructuredOutputMode: OpenAICompatibleStructuredOutputJSON})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Team", Catalog: CapabilityCatalog{}, InvocationKey: "change-set:one:0"})
	if err != nil || !strings.Contains(string(payload), "Which Team") {
		t.Fatalf("payload = %s, err = %v", payload, err)
	}
	if strings.Contains(requestBody, apiKey) || strings.Contains(string(payload), apiKey) {
		t.Fatal("transport credential leaked into model-visible or returned content")
	}
	if strings.Contains(requestBody, "change-set:one:0") {
		t.Fatal("transport idempotency token should not become model-visible prompt data")
	}
}

func TestOpenAICompatibleGeneratorEmitsExplicitThinkingMode(t *testing.T) {
	var observed map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&observed); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer server.Close()

	generator, err := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "deepseek-v4-flash", server.Client(), OpenAICompatibleGeneratorOptions{
		ThinkingMode: OpenAICompatibleThinkingDisabled, StructuredOutputMode: OpenAICompatibleStructuredOutputJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent"}); err != nil {
		t.Fatal(err)
	}
	thinking, ok := observed["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking transport = %#v", observed["thinking"])
	}

	if _, err := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{
		ThinkingMode: "invented",
	}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("invalid thinking mode error = %v", err)
	}
	if _, err := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{
		StructuredOutputMode: "invented",
	}); err == nil || !strings.Contains(err.Error(), "structured output mode") {
		t.Fatalf("invalid structured output mode error = %v", err)
	}
}

func TestOpenAICompatibleGeneratorNegotiatesCanonicalToolTransport(t *testing.T) {
	var observed map[string]interface{}
	arguments := `{"schemaVersion":"openseal.authoring-result/v1","candidate":{"agents":[]},"authoring":{"version":"openseal.authoring-form/v1"},"commitments":{}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&observed); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{
			"finish_reason": "tool_calls", "message": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
				"type": "function", "function": map[string]interface{}{"name": "submit_authoring_result", "arguments": arguments},
			}}},
		}}})
	}))
	defer server.Close()
	generator, err := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{StructuredOutputMode: OpenAICompatibleStructuredOutputTool})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent"})
	if err != nil || string(payload) != arguments {
		t.Fatalf("payload=%s err=%v", payload, err)
	}
	tools := observed["tools"].([]interface{})
	function := tools[0].(map[string]interface{})["function"].(map[string]interface{})
	if function["name"] != "submit_authoring_result" {
		t.Fatalf("function=%#v", function)
	}
	schema := function["parameters"].(map[string]interface{})
	if schema["additionalProperties"] != false {
		t.Fatalf("schema=%#v", schema)
	}
	properties := schema["properties"].(map[string]interface{})
	if properties["schemaVersion"].(map[string]interface{})["const"] != AuthoringResultSchemaVersion {
		t.Fatalf("version=%#v", properties["schemaVersion"])
	}
	if _, exists := observed["response_format"]; exists {
		t.Fatalf("tool mode emitted response_format: %#v", observed)
	}
}

func TestOpenAICompatibleGeneratorSupportsPlainLocallyValidatedTransport(t *testing.T) {
	var observed map[string]interface{}
	content := `{"schemaVersion":"openseal.authoring-result/v1","candidate":{"agents":[]},"authoring":{"version":"openseal.authoring-form/v1"},"commitments":{}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&observed); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{
			"finish_reason": "stop", "message": map[string]interface{}{"content": content},
		}}})
	}))
	defer server.Close()
	generator, err := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{StructuredOutputMode: OpenAICompatibleStructuredOutputPlain})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent"})
	if err != nil || string(payload) != content {
		t.Fatalf("payload=%s err=%v", payload, err)
	}
	if _, exists := observed["tools"]; exists {
		t.Fatalf("plain mode emitted tools: %#v", observed)
	}
	if _, exists := observed["tool_choice"]; exists {
		t.Fatalf("plain mode emitted tool_choice: %#v", observed)
	}
	if _, exists := observed["response_format"]; exists {
		t.Fatalf("plain mode emitted response_format: %#v", observed)
	}
}

func assertStrictAuthoringSchema(t *testing.T, value interface{}, path string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]interface{}:
		if typed["type"] == "object" {
			if typed["additionalProperties"] != false {
				t.Fatalf("%s must set additionalProperties=false: %#v", path, typed)
			}
			properties, _ := typed["properties"].(map[string]interface{})
			required := 0
			switch values := typed["required"].(type) {
			case []string:
				required = len(values)
			case []interface{}:
				required = len(values)
			}
			if required != len(properties) {
				t.Fatalf("%s must require every property: properties=%v required=%v", path, properties, typed["required"])
			}
		}
		for key, child := range typed {
			assertStrictAuthoringSchema(t, child, path+"."+key)
		}
	case []interface{}:
		for index, child := range typed {
			assertStrictAuthoringSchema(t, child, fmt.Sprintf("%s[%d]", path, index))
		}
	}
}

func TestOpenAICompatibleToolTransportRejectsWrongFunction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"type":"function","function":{"name":"other","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()
	generator, err := NewOpenAICompatibleGenerator(server.URL, "secret", "model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent"}); err == nil || !strings.Contains(err.Error(), "invalid submit_authoring_result") {
		t.Fatalf("error=%v", err)
	}
}

func TestOpenAICompatibleGeneratorReportsRefusalWithoutReturningContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"","refusal":"I cannot create that workforce."}}]}`))
	}))
	defer server.Close()

	generator, err := NewOpenAICompatibleGenerator(server.URL, "secret", "model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent"})
	var refusal *ProviderRefusalError
	if !errors.As(err, &refusal) || refusal.Reason != "I cannot create that workforce." {
		t.Fatalf("refusal error = %#v", err)
	}
}

func TestOpenAICompatibleGeneratorRejectsTruncatedCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"content":"{\"candidate\":"}}]}`))
	}))
	defer server.Close()

	generator, err := NewOpenAICompatibleGenerator(server.URL, "secret", "model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent"})
	var incomplete *ProviderIncompleteError
	if !errors.As(err, &incomplete) || incomplete.FinishReason != "length" {
		t.Fatalf("incomplete error = %#v", err)
	}
}

func TestOpenAICompatibleGeneratorCompactsOnlyRedundantCatalogReceipts(t *testing.T) {
	var modelRequest GenerateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Messages) != 2 || json.Unmarshal([]byte(body.Messages[1]["content"]), &modelRequest) != nil {
			t.Fatalf("messages = %#v", body.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"candidate\":{\"agents\":[]},\"commitments\":{}}"}}]}`))
	}))
	defer server.Close()
	exact := capability.NewSkillIdentity("compiled-source", "1.2.3+source.abc", "registry::publisher/source")
	catalog := CapabilityCatalog{
		Skills: map[string]SkillCapability{"source": {
			ID: "source", Version: "1.2.3", SourceIdentity: "registry::publisher/source", Actions: []string{"read"},
			RuntimeIdentity: &exact, ActionContracts: map[string]SkillActionContract{
				"read": {InputSchema: map[string]interface{}{"type": "object"}},
			},
			Readiness: SkillReadinessNeedsBinding, CredentialKinds: []string{"oauth"},
			Compatibility: []SkillCompatibility{
				{Requirement: "action:read", Compatible: true, Evidence: "verified declaration", Reference: "receipt"},
				{Requirement: "credential:oauth", Compatible: false, Evidence: "binding required"},
			},
		}},
		CapabilityNeeds: []CapabilityNeed{{ID: "source-choice", Prompt: "Choose a source", WhyNeeded: "Source is required", SkillIDs: []string{"source"}, Priority: 1}},
		AgentCredentialRequirements: []AgentCredentialRequirement{{
			BindingKey: "MODEL_PROVIDER", DisplayName: "Model provider",
			Prompt: "Choose a model provider.", RequiredForActivation: true,
		}},
		AvailableCredentialGrants: map[string][]capability.OAuth2GrantSummary{
			"SOURCE_CONNECTION": {{
				Provider: "source", Subject: capability.OAuth2SubjectUser, Scopes: []string{"read"},
			}},
		},
	}
	generator, _ := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{StructuredOutputMode: OpenAICompatibleStructuredOutputJSON})
	if _, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a source Agent", Catalog: catalog}); err != nil {
		t.Fatal(err)
	}
	projected := modelRequest.Catalog.Skills["source"]
	if projected.ID != "source" || projected.Version != "1.2.3" || projected.SourceIdentity != "registry::publisher/source" ||
		projected.RuntimeIdentity != nil || len(projected.ActionContracts) != 1 ||
		len(projected.Actions) != 1 || projected.Actions[0] != "read" || projected.Readiness != SkillReadinessNeedsBinding ||
		len(projected.CredentialKinds) != 1 || len(projected.Compatibility) != 1 || projected.Compatibility[0].Requirement != "credential:oauth" {
		t.Fatalf("projected Skill = %#v", projected)
	}
	if len(modelRequest.Catalog.CapabilityNeeds) != 0 {
		t.Fatalf("model-visible capability needs = %#v", modelRequest.Catalog.CapabilityNeeds)
	}
	if len(modelRequest.Catalog.AgentCredentialRequirements) != 0 {
		t.Fatalf("model-visible Agent credential requirements = %#v", modelRequest.Catalog.AgentCredentialRequirements)
	}
	if len(modelRequest.Catalog.AvailableCredentialGrants) != 0 {
		t.Fatalf("model-visible OAuth 2 grants = %#v", modelRequest.Catalog.AvailableCredentialGrants)
	}
	if len(catalog.Skills["source"].Compatibility) != 2 || len(catalog.Skills["source"].ActionContracts) != 1 || len(catalog.CapabilityNeeds) != 1 || len(catalog.AgentCredentialRequirements) != 1 ||
		len(catalog.AvailableCredentialGrants["SOURCE_CONNECTION"]) != 1 {
		t.Fatalf("canonical catalog was mutated = %#v", catalog)
	}
	canonicalBytes, _ := json.Marshal(catalog)
	projectedBytes, _ := json.Marshal(modelRequest.Catalog)
	if len(projectedBytes) >= len(canonicalBytes) {
		t.Fatalf("model catalog was not compacted: canonical=%d projected=%d", len(canonicalBytes), len(projectedBytes))
	}
}

func TestAuthoringPromptKeepsDomainSemanticsWithoutDuplicatingSchema(t *testing.T) {
	prompt := authoringModelSystemPrompt()
	for _, expected := range []string{"Interactive browser work is cognitive", "delegate bounded work to the Agent", "never guess DOM targets", "Objectives are durable outcomes", "Select only exact catalog Skill ids, versions, and actions"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt omitted %q", expected)
		}
	}
	for _, forbidden := range []string{"Return exactly:", "AgentDefinition required fields:", "Each RefinementQuestion is {"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt duplicates schema shape %q", forbidden)
		}
	}
}

func TestAuthoringResultSchemaOwnsRequirednessAndClosedObjects(t *testing.T) {
	schema, err := AuthoringResultJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("root schema=%#v", schema)
	}
	required, _ := schema["required"].([]interface{})
	encoded, _ := json.Marshal(required)
	for _, field := range []string{"schemaVersion", "candidate", "authoring", "commitments"} {
		if !strings.Contains(string(encoded), `"`+field+`"`) {
			t.Fatalf("required=%s missing=%s", encoded, field)
		}
	}
}

func TestAuthoringRepairPromptTreatsSkillSelectionAsAuthoritative(t *testing.T) {
	for _, expected := range []string{
		"An answered server-skill-choice-* refinement is an authoritative operator decision",
		"never retain or substitute another option from that question",
		"Never invent an action absent from the selected catalog Skill",
	} {
		if !strings.Contains(authoringRepairPrompt, expected) {
			t.Fatalf("authoring repair prompt is missing %q", expected)
		}
	}
}

func TestOpenAICompatibleGeneratorRedactsProviderErrorsAndRejectsMultipleChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"provider-secret-detail"}`))
	}))
	defer server.Close()
	generator, _ := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{StructuredOutputMode: OpenAICompatibleStructuredOutputJSON})
	_, err := generator.Generate(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create"})
	if err == nil || strings.Contains(err.Error(), "provider-secret-detail") {
		t.Fatalf("provider error = %v", err)
	}
}

func TestOpenAICompatibleRepairSuppliesMachineReadableViolations(t *testing.T) {
	var messages []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messages = body.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`))
	}))
	defer server.Close()
	generator, err := NewOpenAICompatibleGeneratorWithOptions(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{StructuredOutputMode: OpenAICompatibleStructuredOutputJSON})
	if err != nil {
		t.Fatal(err)
	}
	validation := &AuthoringSchemaValidationError{Violations: []AuthoringSchemaViolation{{Path: "/candidate/agents/0/skillRequirements/0", Message: "missing property skillId"}}}
	if _, err = generator.Repair(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Agent"}, []byte(`{"candidate":{}}`), validation); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages=%#v", messages)
	}
	repair := messages[2]["content"]
	if !strings.Contains(repair, "schemaViolations") || !strings.Contains(repair, "/candidate/agents/0/skillRequirements/0") || !strings.Contains(repair, "Correct only the exact") {
		t.Fatalf("repair prompt=%s", repair)
	}
	if strings.Contains(messages[0]["content"], "Return exactly:") {
		t.Fatal("system prompt duplicates schema shape")
	}
}
