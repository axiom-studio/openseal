package authoring

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthoringIntentAcceptsCompletedToolCallWithStopReason(t *testing.T) {
	valid := `{"schemaVersion":"openseal.authoring-intent/v4","kind":"agent","name":"Slack assistant","purpose":"Help in Slack","agents":[{"key":"slack-assistant","name":"Slack assistant","purpose":"Help in Slack","behavior":"Respond to requests in Slack.","objectives":[{"key":"assist","title":"Help in Slack","outcome":"Requests receive useful responses.","priority":1}]}]}`
	for _, tc := range []struct {
		name, finish, tool, arguments, refusal string
		count                                  int
		wantSuccess                            bool
	}{
		{"qwen completed call", "stop", "submit_authoring_intent", valid, "", 1, true},
		{"standard completed call", "tool_calls", "submit_authoring_intent", valid, "", 1, true},
		{"truncated even with valid JSON", "length", "submit_authoring_intent", valid, "", 1, false},
		{"filtered", "content_filter", "submit_authoring_intent", valid, "", 1, false},
		{"unknown reason", "unknown", "submit_authoring_intent", valid, "", 1, false},
		{"refused", "stop", "submit_authoring_intent", valid, "Request refused", 1, false},
		{"missing call", "stop", "submit_authoring_intent", valid, "", 0, false},
		{"multiple calls", "stop", "submit_authoring_intent", valid, "", 2, false},
		{"wrong tool", "stop", "other_tool", valid, "", 1, false},
		{"invalid schema", "stop", "submit_authoring_intent", `{}`, "", 1, false},
		{"truncated arguments", "stop", "submit_authoring_intent", `{"schemaVersion":`, "", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls := []any{}
				for i := 0; i < tc.count; i++ {
					calls = append(calls, map[string]any{"type": "function", "function": map[string]any{"name": tc.tool, "arguments": tc.arguments}})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"finish_reason": tc.finish,
					"message":       map[string]any{"content": "", "tool_calls": calls, "refusal": tc.refusal},
				}}})
			}))
			defer server.Close()
			generator, err := NewOpenAICompatibleGenerator(server.URL, "test-key", "qwen3.7-flash", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			intent, err := generator.GenerateIntent(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a general purpose Slack bot"})
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("success=%v, want %v; error=%v", err == nil, tc.wantSuccess, err)
			}
			if tc.wantSuccess && (len(intent.Agents) != 1 || intent.Name != "Slack assistant") {
				t.Fatalf("unexpected intent: %#v", intent)
			}
		})
	}
}
