package capability

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeConversationSubjectEvidenceIsStableAndIndependent(t *testing.T) {
	input := conversationSubjectEvidenceAdapter()
	input.SubjectEvidence = []ConversationSubjectEvidence{
		{Action: "send-message", SubjectPath: "receipt.externalConversationId"},
		{Action: "inspect-conversation", SubjectPath: "conversation.externalConversationId"},
	}
	normalized, err := NormalizeConversationAdapter(input)
	if err != nil {
		t.Fatal(err)
	}
	expected := []ConversationSubjectEvidence{
		{Action: "inspect-conversation", SubjectPath: "conversation.externalConversationId"},
		{Action: "send-message", SubjectPath: "receipt.externalConversationId"},
	}
	if !reflect.DeepEqual(normalized.SubjectEvidence, expected) {
		t.Fatalf("subject evidence = %#v", normalized.SubjectEvidence)
	}
	if input.SubjectEvidence[0].Action != "send-message" {
		t.Fatal("normalization reordered its input")
	}
	input.SubjectEvidence[0].SubjectPath = "mutated"
	if !reflect.DeepEqual(normalized.SubjectEvidence, expected) {
		t.Fatal("normalized subject evidence shares its input slice")
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var restored ConversationAdapter
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	restored, err = NormalizeConversationAdapter(restored)
	if err != nil || !reflect.DeepEqual(restored, normalized) {
		t.Fatalf("JSON round trip changed subject evidence: %#v, %v", restored, err)
	}
}

func TestNormalizeConversationSubjectEvidencePreservesAbsentMetadata(t *testing.T) {
	for _, evidence := range [][]ConversationSubjectEvidence{nil, {}} {
		input := conversationSubjectEvidenceAdapter()
		input.SubjectEvidence = evidence
		normalized, err := NormalizeConversationAdapter(input)
		if err != nil {
			t.Fatal(err)
		}
		if normalized.SubjectEvidence != nil {
			t.Fatalf("absent metadata is not canonical: %#v", normalized.SubjectEvidence)
		}
		payload, err := json.Marshal(normalized)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), "subjectEvidence") {
			t.Fatalf("absent evidence was serialized: %s", payload)
		}
	}
}

func TestNormalizeConversationSubjectEvidenceRejectsInvalidDeclarations(t *testing.T) {
	valid := ConversationSubjectEvidence{Action: "inspect", SubjectPath: "conversation.externalConversationId"}
	for name, evidence := range map[string][]ConversationSubjectEvidence{
		"empty action":       {{Action: "", SubjectPath: valid.SubjectPath}},
		"action whitespace":  {{Action: " inspect", SubjectPath: valid.SubjectPath}},
		"unbounded action":   {{Action: strings.Repeat("a", 129), SubjectPath: valid.SubjectPath}},
		"empty path":         {{Action: valid.Action, SubjectPath: ""}},
		"path whitespace":    {{Action: valid.Action, SubjectPath: valid.SubjectPath + " "}},
		"unbounded path":     {{Action: valid.Action, SubjectPath: strings.Repeat("a.", 128) + "b"}},
		"unbounded key":      {{Action: valid.Action, SubjectPath: strings.Repeat("a", 129)}},
		"empty path segment": {{Action: valid.Action, SubjectPath: "conversation..externalConversationId"}},
		"leading dot":        {{Action: valid.Action, SubjectPath: ".externalConversationId"}},
		"trailing dot":       {{Action: valid.Action, SubjectPath: "externalConversationId."}},
		"wildcard":           {{Action: valid.Action, SubjectPath: "conversations.*.externalConversationId"}},
		"array syntax":       {{Action: valid.Action, SubjectPath: "conversations[0].externalConversationId"}},
		"duplicate action":   {valid, valid},
		"ambiguous action":   {valid, {Action: valid.Action, SubjectPath: "other.externalConversationId"}},
	} {
		t.Run(name, func(t *testing.T) {
			adapter := conversationSubjectEvidenceAdapter()
			adapter.SubjectEvidence = evidence
			if _, err := NormalizeConversationAdapter(adapter); err == nil {
				t.Fatal("invalid subject evidence was accepted")
			}
		})
	}
	adapter := conversationSubjectEvidenceAdapter()
	for index := 0; index < 17; index++ {
		adapter.SubjectEvidence = append(adapter.SubjectEvidence, ConversationSubjectEvidence{
			Action: "action-" + strings.Repeat("a", index+1), SubjectPath: "externalConversationId",
		})
	}
	if _, err := NormalizeConversationAdapter(adapter); err == nil {
		t.Fatal("too many subject evidence declarations were accepted")
	}
}

func conversationSubjectEvidenceAdapter() ConversationAdapter {
	return ConversationAdapter{
		ProtocolVersion: ConversationAdapterProtocolV1,
		Name:            "Chat", Description: "Receive and deliver conversations.", Provider: "chat",
		EndpointModes:     []ConversationEndpointMode{ConversationEndpointChannel},
		InboundEventTypes: []string{ConversationEventMessageReceived},
		Delivery: ConversationDeliveryCapabilities{
			Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageSend},
			Ordering:   ConversationDeliveryOrderConversation, Idempotency: IdempotencySupported,
		},
		Transport: ConversationAdapterTransport{
			Kind: "plugin", IngressEndpoint: "chat.ingress", DeliveryEndpoint: "chat.deliver",
		},
	}
}
