package runtime

import (
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestConversationDestinationDiscoveryBuildsBoundedArgumentsAndProjectsOnlyMappedFields(t *testing.T) {
	discovery := capability.ConversationDestinationDiscovery{
		Action: "list", Mode: capability.ConversationEndpointChannel, ItemsPath: "result.channels",
		IDPath: "id", DisplayNamePath: "name", DescriptionPath: "purpose.text",
		CursorArgument: "cursor", LimitArgument: "limit", QueryArgument: "query", NextCursorPath: "result.nextCursor",
	}
	definition := &capability.Definition{ID: "chat", Version: "1", Actions: map[string]capability.Action{"list": {Name: "list"}}}
	adapter := &capability.BoundConversationAdapter{
		Definition: definition, AdapterID: "conversations",
		Adapter: capability.ConversationAdapter{DestinationDiscovery: []capability.ConversationDestinationDiscovery{discovery}},
		Binding: &capability.Binding{Scope: capability.ScopeReference{Kind: "tenant", ID: "7"}, DeploymentID: "agent-1"},
	}
	request := ExternalConversationDestinationDiscoveryRequest{
		Scope: Scope{Kind: "tenant", ID: "7"}, DeploymentID: "agent-1", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Cursor: "cursor-1", Query: "product", Limit: 25,
	}
	resolved, arguments, err := ConversationDestinationDiscoveryArguments(request)
	if err != nil || resolved.Action != "list" || !reflect.DeepEqual(arguments, map[string]interface{}{"cursor": "cursor-1", "query": "product", "limit": 25}) {
		t.Fatalf("discovery = %#v arguments = %#v error = %v", resolved, arguments, err)
	}
	page, err := ProjectConversationDestinationPage(discovery, map[string]interface{}{
		"result": map[string]interface{}{
			"channels": []interface{}{map[string]interface{}{
				"id": "C1", "name": "product-feedback", "purpose": map[string]interface{}{"text": "Customer feedback"},
				"access_token": "must-not-project",
			}},
			"nextCursor": "cursor-2", "providerMetadata": map[string]interface{}{"secret": "must-not-project"},
		},
	})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "C1" || page.Items[0].DisplayName != "product-feedback" ||
		page.Items[0].Description != "Customer feedback" || page.NextCursor != "cursor-2" {
		t.Fatalf("page = %#v error = %v", page, err)
	}
}

func TestConversationDestinationProjectionRejectsDuplicateOrOversizedProviderData(t *testing.T) {
	discovery := capability.ConversationDestinationDiscovery{ItemsPath: "items", IDPath: "id", DisplayNamePath: "name"}
	for name, output := range map[string]map[string]interface{}{
		"duplicate": {"items": []interface{}{map[string]interface{}{"id": "one", "name": "One"}, map[string]interface{}{"id": "one", "name": "Again"}}},
		"invalid":   {"items": []interface{}{map[string]interface{}{"id": "one", "name": "line\nbreak"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ProjectConversationDestinationPage(discovery, output); err == nil {
				t.Fatal("invalid provider destination output was accepted")
			}
		})
	}
}
