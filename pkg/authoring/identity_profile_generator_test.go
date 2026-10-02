package authoring

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
)

const validIdentityProfileJSON = `{"name":"Nori","purpose":"Help with useful research","behavior":"Help resourcefully and discover relevant Skills during conversation.","personality":"Warm and curious","operatingPrinciples":["Be clear about uncertainty"],"clarifications":[]}`

func identityProfileProviderEnvelope(responses bool, mode OpenAICompatibleStructuredOutputMode, profile string) map[string]interface{} {
	if responses {
		if mode == OpenAICompatibleStructuredOutputTool || mode == OpenAICompatibleStructuredOutputDefault {
			return map[string]interface{}{"status": "completed", "output": []interface{}{map[string]interface{}{
				"type": "function_call", "name": "submit_agent_profile", "arguments": profile,
			}}}
		}
		return map[string]interface{}{"status": "completed", "output": []interface{}{map[string]interface{}{
			"type": "message", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": profile}},
		}}}
	}
	message := map[string]interface{}{"content": profile}
	finish := "stop"
	if mode == OpenAICompatibleStructuredOutputTool || mode == OpenAICompatibleStructuredOutputDefault {
		message = map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
			"type": "function", "function": map[string]interface{}{"name": "submit_agent_profile", "arguments": profile},
		}}}
		finish = "tool_calls"
	}
	return map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": finish, "message": message}}}
}

func TestIdentityProfileGeneratorCompactSingleRequestAcrossTransports(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, mode := range []OpenAICompatibleStructuredOutputMode{
			OpenAICompatibleStructuredOutputDefault, OpenAICompatibleStructuredOutputTool,
			OpenAICompatibleStructuredOutputJSON, OpenAICompatibleStructuredOutputPlain,
		} {
			name := "chat/" + string(mode)
			if responses {
				name = "responses/" + string(mode)
			}
			t.Run(name, func(t *testing.T) {
				requests := 0
				var observed map[string]interface{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					requests++
					if request.Header.Get("Authorization") != "Bearer private-provider-key" || request.Header.Get("Idempotency-Key") != "private-invocation-key" {
						t.Error("provider transport omitted its private authorization or idempotency header")
					}
					if err := json.NewDecoder(request.Body).Decode(&observed); err != nil {
						t.Error(err)
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(identityProfileProviderEnvelope(responses, mode, validIdentityProfileJSON))
				}))
				defer server.Close()
				constructor := NewOpenAICompatibleGeneratorWithOptions
				if responses {
					constructor = NewOpenAIResponsesGeneratorWithOptions
				}
				generator, err := constructor(server.URL, "private-provider-key", "model", server.Client(), OpenAICompatibleGeneratorOptions{StructuredOutputMode: mode})
				if err != nil {
					t.Fatal(err)
				}
				question := RefinementQuestion{
					ID: "profile-personality", Category: RefinementCategoryOther,
					Prompt: "Which tone should it use?", WhyNeeded: "The supplied tones conflict.",
					Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerText},
					Priority: 500, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
				}
				request := GenerateRequest{
					ProfileOnly: true, Mode: ModeAmend, Prompt: "Create a friendly research companion", AgentName: "Nori",
					ExistingAgentNames: []string{"Finn", "Pebble"}, InvocationKey: "private-invocation-key",
					Existing: &WorkforceCandidate{Agents: []*agent.AgentDefinition{{
						ID: "private-runtime-id", Version: "99.0.0", DisplayName: "Nori", Purpose: "Help with research",
						SystemPrompt: "Investigate carefully", Personality: "Curious", OperatingPrinciples: []string{"Check evidence"},
					}}},
					Catalog:                 CapabilityCatalog{Skills: map[string]SkillCapability{"private-skill-inventory": {Description: "Do not expose inventory"}}, AvailableCredentials: map[string]bool{"private-account-kind": true}},
					Form:                    AuthoringForm{Fields: []AuthoringFormField{{ID: "private-workflow-form"}}},
					CompositionRequirements: &RuntimeCompositionRequirements{},
					Refinement: &RefinementContext{Questions: []RefinementQuestion{question}, Answers: []RefinementResolvedAnswer{
						{QuestionID: question.ID, Value: RefinementProviderAnswerValue{Text: "Warm and curious", CredentialKind: "private-credential-kind", SkillIDs: []string{"private-refinement-skill"}}},
						{QuestionID: "server-account-choice", Value: RefinementProviderAnswerValue{Text: "private-setup-answer", CredentialConfigured: true}},
					}},
				}
				profile, err := generator.GenerateProfile(t.Context(), request)
				if err != nil || profile.Name != "Nori" || profile.Personality != "Warm and curious" {
					t.Fatalf("profile=%#v error=%v", profile, err)
				}
				if requests != 1 {
					t.Fatalf("profile generation made %d provider requests", requests)
				}
				encoded, _ := json.Marshal(observed)
				for _, forbidden := range []string{
					"private-provider-key", "private-invocation-key", "private-runtime-id", "99.0.0", "private-skill-inventory",
					"private-account-kind", "private-workflow-form", "private-credential-kind", "private-refinement-skill", "private-setup-answer",
					"submit_authoring_intent", "submit_authoring_result", "compositionRequirements", "authoringForm",
				} {
					if strings.Contains(string(encoded), forbidden) {
						t.Errorf("profile provider request exposed %q", forbidden)
					}
				}
				var messages []interface{}
				if responses {
					messages = observed["input"].([]interface{})
				} else {
					messages = observed["messages"].([]interface{})
				}
				var user map[string]interface{}
				for _, raw := range messages {
					message := raw.(map[string]interface{})
					if message["role"] == "user" {
						if err := json.Unmarshal([]byte(message["content"].(string)), &user); err != nil {
							t.Fatal(err)
						}
					}
				}
				if len(user) != 5 || user["agentName"] != "Nori" || user["existing"] == nil || user["answers"] == nil {
					t.Fatalf("profile input is not the compact identity projection: %#v", user)
				}
				answers := user["answers"].([]interface{})
				wantAnswer := map[string]interface{}{"field": "personality", "question": question.Prompt, "text": "Warm and curious"}
				if len(answers) != 1 || !reflect.DeepEqual(answers[0], wantAnswer) {
					t.Fatalf("identity answer projection=%#v", answers)
				}
				if mode == OpenAICompatibleStructuredOutputTool || mode == OpenAICompatibleStructuredOutputDefault {
					tools := observed["tools"].([]interface{})
					function := tools[0].(map[string]interface{})
					if !responses {
						function = function["function"].(map[string]interface{})
					}
					if function["name"] != "submit_agent_profile" {
						t.Fatalf("profile tool=%#v", function)
					}
					assertIdentityProfileProviderSchema(t, function["parameters"].(map[string]interface{}))
				}
			})
		}
	}
}

func assertIdentityProfileProviderSchema(t *testing.T, schema map[string]interface{}) {
	t.Helper()
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("profile schema is not a closed object: %#v", schema)
	}
	properties := schema["properties"].(map[string]interface{})
	if len(properties) != 6 {
		t.Fatalf("profile contract has additional fields: %#v", properties)
	}
	for _, required := range []string{"name", "purpose", "behavior", "personality", "operatingPrinciples", "clarifications"} {
		if properties[required] == nil {
			t.Errorf("profile schema omitted %s", required)
		}
	}
	clarification := properties["clarifications"].(map[string]interface{})["items"].(map[string]interface{})
	if clarification["additionalProperties"] != false {
		t.Fatal("profile clarification permits workflow or setup fields")
	}
}

func TestIdentityProfileGeneratorRejectsWorkflowWithoutSchemaRepair(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		invalid := strings.TrimSuffix(validIdentityProfileJSON, "}") + `,"workflow":{"steps":[]}}`
		_ = json.NewEncoder(w).Encode(identityProfileProviderEnvelope(false, OpenAICompatibleStructuredOutputDefault, invalid))
	}))
	defer server.Close()
	generator, err := NewOpenAICompatibleGenerator(server.URL, "secret", "model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.GenerateProfile(t.Context(), GenerateRequest{ProfileOnly: true, Prompt: "Create a researcher"})
	if err == nil || !strings.Contains(err.Error(), `unknown field "workflow"`) || requests != 1 {
		t.Fatalf("error=%v provider requests=%d", err, requests)
	}
}

func TestIdentityProfileGeneratorPreservesProviderRefusalAndIncomplete(t *testing.T) {
	for _, test := range []struct {
		name       string
		responses  bool
		body       string
		refusal    bool
		incomplete bool
	}{
		{name: "chat refusal", body: `{"choices":[{"finish_reason":"stop","message":{"refusal":"Cannot create that profile"}}]}`, refusal: true},
		{name: "chat incomplete", body: `{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`, incomplete: true},
		{name: "responses refusal", responses: true, body: `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"Cannot create that profile"}]}]}`, refusal: true},
		{name: "responses incomplete", responses: true, body: `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, incomplete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests++
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			constructor := NewOpenAICompatibleGeneratorWithOptions
			if test.responses {
				constructor = NewOpenAIResponsesGeneratorWithOptions
			}
			generator, err := constructor(server.URL, "secret", "model", server.Client(), OpenAICompatibleGeneratorOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = generator.GenerateProfile(t.Context(), GenerateRequest{ProfileOnly: true, Prompt: "Create a researcher"})
			var refusal *ProviderRefusalError
			var incomplete *ProviderIncompleteError
			if test.refusal && !errors.As(err, &refusal) || test.incomplete && !errors.As(err, &incomplete) || requests != 1 {
				t.Fatalf("provider outcome=%v provider requests=%d", err, requests)
			}
		})
	}
}
