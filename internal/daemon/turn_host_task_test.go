package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestProviderTurnHostProjectsTrustedTaskAuthority(t *testing.T) {
	for _, mode := range []string{"untrusted", "read-only", "can-start", "task-worker"} {
		t.Run(mode, func(t *testing.T) {
			request := runtime.HostedTurnRequest{
				APIVersion: runtime.HostedTurnAPIVersion, InvocationID: "turn", Goal: "Reply", AgentID: "agent",
				InputContext:   map[string]interface{}{"conversationTasks": map[string]interface{}{"canStart": true}},
				EligibleAgents: []runtime.HostedAgentTarget{{ID: "agent", DisplayName: "Current Agent"}},
			}
			if mode != "untrusted" {
				request.ConversationTasks = &runtime.HostedConversationTaskContext{ConversationID: "conversation", CanStart: mode == "can-start"}
			}
			if mode == "task-worker" {
				request.InputContext[runtime.ConversationTaskContextKey] = "task-hint"
			}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				tool := body["tools"].([]interface{})[0].(map[string]interface{})["function"].(map[string]interface{})
				properties := tool["parameters"].(map[string]interface{})["properties"].(map[string]interface{})
				_, offered := properties["proposedTask"]
				if offered != (mode == "can-start") {
					t.Errorf("task proposal offered=%t for %s", offered, mode)
				}
				_, delegationOffered := properties["proposedDelegation"]
				_, forkOffered := properties["proposedFork"]
				if delegationOffered != (mode != "task-worker") || !forkOffered {
					t.Errorf("wrong work offers for %s: delegation=%t fork=%t", mode, delegationOffered, forkOffered)
				}
				messages := body["messages"].([]interface{})
				var modelInput map[string]interface{}
				if err := json.Unmarshal([]byte(messages[1].(map[string]interface{})["content"].(string)), &modelInput); err != nil {
					t.Error(err)
				}
				_, projected := modelInput["conversationTasks"]
				if projected != (mode != "untrusted") {
					t.Errorf("trusted task context projected=%t for %s", projected, mode)
				}
				form, err := json.Marshal(map[string]interface{}{
					"schemaVersion": runtime.HostedTurnFormSchemaVersion, "nextRunStatus": "completed",
					"outputSummary": "Ready", "runOutput": map[string]string{"reply": "Ready"},
				})
				if err != nil {
					t.Error(err)
				}
				json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{
					"finish_reason": "tool_calls", "message": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
						"type": "function", "function": map[string]string{"name": "submit_agent_turn", "arguments": string(form)},
					}}},
				}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
			}))
			defer provider.Close()
			host, err := NewProviderTurnHost(provider.URL, "key", "model")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := host.ExecuteHostedTurn(t.Context(), request); err != nil {
				t.Fatal(err)
			}
		})
	}
}
