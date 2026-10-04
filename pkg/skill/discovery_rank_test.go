package skill

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func rankedDiscoveryTestCandidate(id string) DiscoveryCandidate {
	return DiscoveryCandidate{
		ID: id, Version: "1.0.0", SourceIdentity: "https://example.test/skills::" + id,
		Name: id, Readiness: DiscoveryReadinessBindable,
		Actions: []DiscoveryAction{
			{Name: " z-action ", Description: " Last action ", Risk: RiskLevelRead},
			{Name: " a-action ", Description: " First action ", Risk: RiskLevelRead},
		},
		Credentials: []DiscoveryCredential{
			{Name: " z-credential ", Kind: " token "},
			{Name: " a-credential ", Kind: " oauth ", OAuth2: &OAuth2Requirement{
				Provider: " example ", Subject: OAuth2SubjectUser, Scopes: []string{"write", "read"},
			}},
		},
		ConversationAdapters: []DiscoveryConversationAdapter{{
			ID: " messages ", Provider: " example ", ProtocolVersion: ConversationAdapterProtocolV1,
			EndpointModes:                []ConversationEndpointMode{ConversationEndpointDirect, ConversationEndpointChannel},
			InboundEventTypes:            []string{ConversationEventMessageUpdated, ConversationEventMessageReceived},
			Features:                     []ConversationAdapterFeature{ConversationFeatureThreads, ConversationFeatureAttachments},
			DiscoverableDestinationModes: []ConversationEndpointMode{ConversationEndpointDirect, ConversationEndpointChannel},
			Delivery: ConversationDeliveryCapabilities{
				Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageUpdate, ConversationDeliveryMessageSend},
				Ordering:   ConversationDeliveryOrderThread, Idempotency: IdempotencyRequired,
			},
			Credentials: []DiscoveryCredential{
				{Name: " z-connection ", Kind: " token "},
				{Name: " a-connection ", Kind: " oauth ", OAuth2: &OAuth2Requirement{
					Provider: "example", Subject: OAuth2SubjectUser, Scopes: []string{"write", "read"},
				}},
			},
		}},
		BindingConfigSchema: map[string]interface{}{
			"type": "object", "required": []interface{}{"destination"},
			"properties": map[string]interface{}{"destination": map[string]interface{}{"type": "string"}},
		},
		Compatibility: []DiscoveryCompatibility{
			{Requirement: " runtime ", Compatible: true, Evidence: " available ", Reference: " receipt:z "},
			{Requirement: " runtime ", Compatible: false, Evidence: " unavailable ", Reference: " receipt:b "},
			{Requirement: " installation ", Compatible: true, Evidence: " installed ", Reference: " receipt:i "},
			{Requirement: " runtime ", Compatible: false, Evidence: " unavailable ", Reference: " receipt:a "},
		},
	}
}

func encodeDiscoveryTestValue(t *testing.T, value interface{}) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestDiscoveryPreservesProviderRankingAcrossPages(t *testing.T) {
	request := DiscoveryRequest{
		Scope: ScopeReference{Kind: "tenant", ID: "tenant-a"}, DeploymentID: "agent",
		Query: "read recent messages", Limit: 2,
	}
	pages := []*DiscoveryPage{
		{Items: []DiscoveryCandidate{rankedDiscoveryTestCandidate("z-best"), rankedDiscoveryTestCandidate("b-next")}, NextCursor: "ranked-page-2"},
		{Items: []DiscoveryCandidate{rankedDiscoveryTestCandidate("a-third")}},
	}
	provider := DiscoveryProviderFunc(func(_ context.Context, input DiscoveryRequest) (*DiscoveryPage, error) {
		if input.Cursor == "ranked-page-2" {
			return pages[1], nil
		}
		return pages[0], nil
	})
	var ids []string
	for pageIndex := range pages {
		page, err := provider.DiscoverSkills(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := NormalizeDiscoveryPage(request, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range normalized.Items {
			ids = append(ids, candidate.ID)
		}
		if normalized.NextCursor != page.NextCursor {
			t.Fatal("normalization replaced the provider's pagination cursor")
		}
		request.Cursor = normalized.NextCursor
		if pageIndex == 0 && normalized.Items[0].Actions[0].Name != "a-action" {
			t.Fatal("preserving relevance disabled nested action normalization")
		}
	}
	if !reflect.DeepEqual(ids, []string{"z-best", "b-next", "a-third"}) {
		t.Fatalf("provider relevance order was replaced with alphabetical order: %v", ids)
	}
}

func TestDiscoveryNormalizationOwnsNestedDataAndDoesNotMutateProvider(t *testing.T) {
	input := &DiscoveryPage{Items: []DiscoveryCandidate{rankedDiscoveryTestCandidate("z-best")}, NextCursor: "page-2"}
	before := encodeDiscoveryTestValue(t, input)
	output, err := NormalizeDiscoveryPage(DiscoveryRequest{Limit: 1}, input)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, encodeDiscoveryTestValue(t, input)) {
		t.Fatal("normalization mutated the provider's catalog snapshot")
	}
	candidate := &output.Items[0]
	candidate.Actions[0].Name = "changed"
	candidate.Credentials[0].Name = "changed"
	candidate.Credentials[0].OAuth2.Scopes[0] = "changed"
	candidate.Compatibility[0].Evidence = "changed"
	adapter := &candidate.ConversationAdapters[0]
	adapter.ID = "changed"
	adapter.EndpointModes[0] = "changed"
	adapter.InboundEventTypes[0] = "changed"
	adapter.Features[0] = "changed"
	adapter.DiscoverableDestinationModes[0] = "changed"
	adapter.Delivery.Operations[0] = "changed"
	adapter.Credentials[0].Name = "changed"
	adapter.Credentials[0].OAuth2.Scopes[0] = "changed"
	candidate.BindingConfigSchema["required"].([]interface{})[0] = "changed"
	candidate.BindingConfigSchema["properties"].(map[string]interface{})["destination"].(map[string]interface{})["type"] = "changed"
	if !slices.Equal(before, encodeDiscoveryTestValue(t, input)) {
		t.Fatal("normalized output retains aliases into the provider's snapshot")
	}
}

func TestDiscoveryCompatibilityEvidenceProducesStableReplay(t *testing.T) {
	request := DiscoveryRequest{Limit: 2}
	first := &DiscoveryPage{Items: []DiscoveryCandidate{rankedDiscoveryTestCandidate("z-best"), rankedDiscoveryTestCandidate("a-next")}}
	second := &DiscoveryPage{Items: []DiscoveryCandidate{rankedDiscoveryTestCandidate("z-best"), rankedDiscoveryTestCandidate("a-next")}}
	for index := range second.Items {
		slices.Reverse(second.Items[index].Compatibility)
		slices.Reverse(second.Items[index].Actions)
		slices.Reverse(second.Items[index].Credentials)
	}
	left, err := NormalizeDiscoveryPage(request, first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NormalizeDiscoveryPage(request, second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(encodeDiscoveryTestValue(t, left), encodeDiscoveryTestValue(t, right)) {
		t.Fatal("equivalent map-derived compatibility evidence produced different replay bytes")
	}
	evidence := left.Items[0].Compatibility
	if evidence[0].Requirement != "installation" || evidence[1].Compatible || evidence[1].Reference != "receipt:a" ||
		evidence[2].Reference != "receipt:b" || !evidence[3].Compatible {
		t.Fatalf("compatibility evidence was not canonicalized as a complete tuple: %+v", evidence)
	}
	replayed, err := NormalizeDiscoveryPage(request, left)
	if err != nil || !slices.Equal(encodeDiscoveryTestValue(t, left), encodeDiscoveryTestValue(t, replayed)) {
		t.Fatalf("replaying a normalized ranked page was not idempotent: %v", err)
	}
}

func TestDiscoveryInvalidProviderDataStillFailsWithoutMutatingSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DiscoveryCandidate)
	}{
		{"invalid action risk", func(value *DiscoveryCandidate) { value.Actions[1].Risk = "invented" }},
		{"repeated action", func(value *DiscoveryCandidate) { value.Actions[1].Name = value.Actions[0].Name }},
		{"invalid OAuth contract", func(value *DiscoveryCandidate) { value.Credentials[1].OAuth2.Subject = "invented" }},
		{"invalid adapter", func(value *DiscoveryCandidate) { value.ConversationAdapters[0].ProtocolVersion = "invented" }},
		{"invalid compatibility", func(value *DiscoveryCandidate) { value.Compatibility[1].Evidence = " " }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := &DiscoveryPage{Items: []DiscoveryCandidate{rankedDiscoveryTestCandidate("z-best")}}
			test.mutate(&input.Items[0])
			before := encodeDiscoveryTestValue(t, input)
			if output, err := NormalizeDiscoveryPage(DiscoveryRequest{Limit: 1}, input); err == nil || output != nil {
				t.Fatalf("malformed provider candidate accepted: %v %v", output, err)
			}
			if !slices.Equal(before, encodeDiscoveryTestValue(t, input)) {
				t.Fatal("failed normalization mutated the provider's snapshot")
			}
		})
	}
}
