package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func TestDesktopConversationRouterForcesRouteMessageThroughSharedClient(t *testing.T) {
	var body map[string]interface{}
	provider := newFakeProvider(t, func(_ int, w http.ResponseWriter, got map[string]interface{}) {
		body = got
		chatReply(w, "tool_calls", map[string]interface{}{"tool_calls": []interface{}{
			chatCall("r1", "route_message", `{"agentId":" press ","reason":" owns the press list "}`),
		}}, nil)
	})
	host := provider.host(t, 0)
	router, err := NewDesktopConversationRouter(host)
	if err != nil {
		t.Fatal(err)
	}
	if router.client != host.client {
		t.Fatal("router does not share the step host's client")
	}
	decision, err := router.Route(context.Background(), conversation.ConversationRouteRequest{
		Lead:    "lead",
		Members: []conversation.RouteMember{{AgentID: "lead", Name: "Lead"}, {AgentID: "press", Name: "Press"}},
		Trigger: models.ChannelMessage{ID: "m1", Content: strings.Repeat("a", 2500)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.AgentID != "press" || decision.Reason != "owns the press list" {
		t.Fatalf("decision: %+v", decision)
	}
	if body["model"] != stepModel || body["max_completion_tokens"] != float64(256) {
		t.Fatalf("body: %v", body)
	}
	choice, _ := json.Marshal(body["tool_choice"])
	if string(choice) != `{"function":{"name":"route_message"},"type":"function"}` {
		t.Fatalf("tool_choice %s", choice)
	}
	tools := body["tools"].([]interface{})
	parameters := tools[0].(map[string]interface{})["function"].(map[string]interface{})["parameters"].(map[string]interface{})
	enum, _ := json.Marshal(parameters["properties"].(map[string]interface{})["agentId"].(map[string]interface{})["enum"])
	if len(tools) != 1 || string(enum) != `["lead","press"]` {
		t.Fatalf("tools: %v", tools)
	}
	messages := body["messages"].([]interface{})
	var input map[string]interface{}
	_ = json.Unmarshal([]byte(messages[1].(map[string]interface{})["content"].(string)), &input)
	if content := input["message"].(map[string]interface{})["content"].(string); len(content) != 2000 {
		t.Fatalf("trigger content not bounded: %d", len(content))
	}

	failing := newFakeProvider(t, func(_ int, w http.ResponseWriter, _ map[string]interface{}) { w.WriteHeader(502) })
	router, _ = NewDesktopConversationRouter(failing.host(t, 0))
	if _, err := router.Route(context.Background(), conversation.ConversationRouteRequest{Members: []conversation.RouteMember{{AgentID: "a"}}}); err == nil || err.Error() != "the routing provider returned HTTP 502" {
		t.Fatalf("http error: %v", err)
	}
	if _, err := NewDesktopConversationRouter(nil); err == nil {
		t.Fatal("accepted a nil provider")
	}
}
