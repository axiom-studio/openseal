package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

type fixedExternalConversationRunbookResolver struct {
	resolved *ResolvedExternalConversationRunbook
}

func (r fixedExternalConversationRunbookResolver) ResolveExternalConversationRunbook(
	context.Context,
	Scope,
	*ExternalConversationEndpoint,
	ExternalConversationHandler,
) (*ResolvedExternalConversationRunbook, error) {
	return r.resolved, nil
}

func TestExternalConversationRunbookEventDispatcherCreatesOneExactEventRun(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	handler := ExternalConversationHandler{
		Kind: ExternalConversationHandlerRunbook, ID: "support-chat", Version: "2.0.0", Trigger: "on-message",
		AssignedAgentID: "support-agent",
	}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: handler.ID, Version: handler.Version, Name: "Support chat",
		Entrypoints: map[string]string{"respond": "done"},
		Interfaces: map[string]runbook.Interface{"respond": {
			Description: "Respond to a canonical conversation message.",
			InputSchema: map[string]interface{}{
				"type": "object", "required": []interface{}{"conversationId", "triggerMessageId", "event"},
				"properties": map[string]interface{}{
					"conversationId":   map[string]interface{}{"type": "string"},
					"triggerMessageId": map[string]interface{}{"type": "string"},
					"event":            map[string]interface{}{"type": "object"},
				},
			},
		}},
		Triggers: map[string]runbook.Trigger{"on-message": {
			Kind: runbook.TriggerEvent, EventType: externalConversationEventType, Entrypoint: "respond", ObjectiveID: "agent:support-agent:respond",
		}},
		Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
	}
	dispatcher := NewExternalConversationRunbookEventDispatcher(
		store,
		fixedExternalConversationRunbookResolver{resolved: &ResolvedExternalConversationRunbook{
			Definition: definition, AssignedAgentID: "support-agent",
		}},
	)
	endpoint := &ExternalConversationEndpoint{
		ID: "support-endpoint", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "support-team"},
		Handler: handler,
	}
	conversation := &Conversation{ID: "conversation"}
	message := &ChannelMessage{ID: "message"}
	event := EventEnvelope{
		ID: "event", Scope: scope, Type: externalConversationEventType, Source: "conversation-adapter:webchat",
		Subject: conversation.ID, OccurredAt: time.Now().UTC(),
		Attributes: map[string]interface{}{"endpointId": endpoint.ID},
		Payload:    map[string]interface{}{"conversationId": conversation.ID, "messageId": message.ID},
	}
	request := ExternalConversationDispatchRequest{
		Endpoint: endpoint, Conversation: conversation, Message: message, Event: event,
		IdempotencyKey: "external-conversation-dispatch:event",
	}
	first, err := dispatcher.DispatchExternalConversationRunbook(t.Context(), handler, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := dispatcher.DispatchExternalConversationRunbook(t.Context(), handler, request)
	if err != nil || replayed.RunID != first.RunID {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("Runs = %#v, %v", runs, err)
	}
	run := runs[0]
	runbookPlan, _ := run.Plan["runbook"].(map[string]interface{})
	if run.Kind != RunKindConversation || run.Source != RunSourceEvent || run.Owner != endpoint.Owner ||
		run.AssignedAgentID != "support-agent" || run.Entrypoint != "respond" ||
		run.ConcurrencyKey != conversation.ID || run.Context["triggerMessageId"] != message.ID ||
		runbookPlan["id"] != definition.ID || runbookPlan["version"] != definition.Version ||
		runbookPlan["trigger"] != handler.Trigger {
		t.Fatalf("Run = %#v", run)
	}
}

func TestExternalConversationRunbookEventDispatcherRejectsTriggerDrift(t *testing.T) {
	store := NewMemoryStore()
	handler := ExternalConversationHandler{
		Kind: ExternalConversationHandlerRunbook, ID: "support-chat", Version: "1", Trigger: "on-message",
		AssignedAgentID: "support-agent",
	}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: handler.ID, Version: handler.Version, Name: "Support",
		Entrypoints: map[string]string{"respond": "done"},
		Triggers: map[string]runbook.Trigger{"on-message": {
			Kind: runbook.TriggerEvent, EventType: "incident.created", Entrypoint: "respond", ObjectiveID: "agent:support-agent:respond",
		}},
		Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
	}
	dispatcher := NewExternalConversationRunbookEventDispatcher(
		store,
		fixedExternalConversationRunbookResolver{resolved: &ResolvedExternalConversationRunbook{
			Definition: definition, AssignedAgentID: "support-agent",
		}},
	)
	scope := Scope{Kind: "tenant", ID: "one"}
	endpoint := &ExternalConversationEndpoint{
		ID: "endpoint", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "support-agent"}, Handler: handler,
	}
	_, err := dispatcher.DispatchExternalConversationRunbook(t.Context(), handler, ExternalConversationDispatchRequest{
		Endpoint: endpoint, Conversation: &Conversation{ID: "conversation"}, Message: &ChannelMessage{ID: "message"},
		Event: EventEnvelope{
			ID: "event", Scope: scope, Type: externalConversationEventType,
			Source: "conversation-adapter:webchat", OccurredAt: time.Now().UTC(),
		},
		IdempotencyKey: "dispatch",
	})
	if err == nil {
		t.Fatal("trigger drift was accepted")
	}
}
