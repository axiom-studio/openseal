package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func jsonSchemaTransportRequest(t *testing.T, r *http.Request, name, schemaProperty string) {
	t.Helper()
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
		return
	}
	if body["tools"] != nil || body["tool_choice"] != nil {
		t.Errorf("json_schema transport sent tools: %v %v", body["tools"], body["tool_choice"])
	}
	format, _ := body["response_format"].(map[string]interface{})
	jsonSchema, _ := format["json_schema"].(map[string]interface{})
	schema, _ := jsonSchema["schema"].(map[string]interface{})
	if branches, ok := schema["oneOf"].([]interface{}); ok {
		schema, _ = branches[0].(map[string]interface{})
	}
	properties, _ := schema["properties"].(map[string]interface{})
	if format["type"] != "json_schema" || jsonSchema["name"] != name || properties[schemaProperty] == nil {
		t.Errorf("response_format = %#v", format)
	}
	system := body["messages"].([]interface{})[0].(map[string]interface{})["content"].(string)
	if !strings.Contains(system, `"`+schemaProperty+`"`) {
		t.Error("json_schema transport did not state the reply schema to the model")
	}
}

func TestProviderTurnHostJSONSchemaTransportDecodesContentStrictly(t *testing.T) {
	valid, _ := json.Marshal(map[string]interface{}{"schemaVersion": runtime.HostedTurnFormSchemaVersion, "nextRunStatus": "completed", "outputSummary": "Analysis complete", "runOutput": map[string]string{"reply": "Four"}})
	toolCall := []interface{}{map[string]interface{}{"type": "function", "function": map[string]string{"name": "submit_agent_turn", "arguments": string(valid)}}}
	for _, test := range []struct {
		name    string
		finish  string
		message map[string]interface{}
		code    string
	}{
		{name: "content with stop", finish: "stop", message: map[string]interface{}{"content": string(valid)}},
		{name: "unknown field", finish: "stop", message: map[string]interface{}{"content": strings.Replace(string(valid), `{`, `{"unknownAuthority":true,`, 1)}, code: "invalid_turn"},
		{name: "invalid JSON", finish: "stop", message: map[string]interface{}{"content": `{"schemaVersion":`}, code: "invalid_turn"},
		{name: "truncated", finish: "length", message: map[string]interface{}{"content": string(valid)}, code: "incomplete_response"},
		{name: "tool call instead of content", finish: "tool_calls", message: map[string]interface{}{"tool_calls": toolCall}, code: "incomplete_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jsonSchemaTransportRequest(t, r, "submit_agent_turn", "runOutput")
				json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": test.finish, "message": test.message}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
			}))
			defer provider.Close()
			host, err := NewProviderTurnHostWithOptions(provider.URL, "key", "model", ProviderTurnHostOptions{StructuredOutput: ProviderStructuredOutputJSONSchema})
			if err != nil {
				t.Fatal(err)
			}
			response, err := host.ExecuteHostedTurn(t.Context(), runtime.HostedTurnRequest{APIVersion: runtime.HostedTurnAPIVersion, InvocationID: "turn-1", Goal: "Two plus two"})
			if test.code == "" {
				if err != nil || response.RunOutput["reply"] != "Four" || response.Usage.OutputTokens != 5 {
					t.Fatalf("result: %+v %v", response, err)
				}
				return
			}
			var failure *runtime.TurnHostFailure
			if !errors.As(err, &failure) || failure.Code != test.code {
				t.Fatalf("error = %v, want code %s", err, test.code)
			}
		})
	}
}

func TestDesktopParticipationProviderJSONSchemaTransport(t *testing.T) {
	catalog, query := participationFixture()
	source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 2)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonSchemaTransportRequest(t, r, "submit_channel_contribution", "wantsToSpeak")
		content := `{"wantsToSpeak":true,"content":"Check the source first.","roleRelevant":true,"hasNewInformation":true}`
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": "stop", "message": map[string]interface{}{"content": content}}}, "usage": map[string]int{"prompt_tokens": 40, "completion_tokens": 20}})
	}))
	defer server.Close()
	host, err := NewProviderTurnHostWithOptions(server.URL, "key", "model", ProviderTurnHostOptions{StructuredOutput: ProviderStructuredOutputJSONSchema})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewDesktopParticipationProvider(host, source)
	if err != nil {
		t.Fatal(err)
	}
	participant := runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "a"}
	trigger := &runtime.ChannelMessage{ID: "trigger", Scope: query.Conversation.Scope, ConversationID: query.Conversation.ID, Intent: runtime.MessageIntentQuestion, Content: "Review the evidence", Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceParticipants, Participants: []runtime.ConversationParticipant{participant}}}
	report, err := provider.ProposeParticipationWithUsage(t.Context(), runtime.ParticipationProposalContext{Conversation: query.Conversation, Trigger: trigger, Participant: participant, SemanticRoles: []string{"a"}, Budget: &runtime.ParticipationProposalBudget{InputTokens: 10000, OutputTokens: 100}})
	if err != nil || !report.Proposal.WantsToSpeak || report.Proposal.Content != "Check the source first." {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestNewProviderTurnHostRejectsUnknownStructuredOutput(t *testing.T) {
	if _, err := NewProviderTurnHostWithOptions("http://127.0.0.1:1/v1", "key", "model", ProviderTurnHostOptions{StructuredOutput: "json"}); err == nil || !strings.Contains(err.Error(), `structured output mode "json"`) {
		t.Fatalf("error = %v", err)
	}
	host, err := NewProviderTurnHost("http://127.0.0.1:1/v1", "key", "model")
	if err != nil || host.structuredOutput != ProviderStructuredOutputTool {
		t.Fatalf("default transport = %v err=%v", host, err)
	}
}
