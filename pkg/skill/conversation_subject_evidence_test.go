package skill

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConversationSubjectEvidenceAcceptsGovernedSendReceipts(t *testing.T) {
	definition := conversationSubjectEvidenceSkill()
	if err := validateDefinition(definition); err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	legacy := conversationAdapterOnlySkill()
	if err := validateDefinition(legacy); err != nil {
		t.Fatalf("adapter without evidence is invalid: %v", err)
	}
}

func TestConversationSubjectEvidenceRequiresAnOutputStringAndCompatibleCredentials(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*Definition)
		want   string
	}{
		"missing action": {func(value *Definition) { delete(value.Actions, "send-message") }, "missing action"},
		"missing output": {func(value *Definition) {
			action := value.Actions["send-message"]
			action.OutputSchema = nil
			value.Actions["send-message"] = action
		}, "declared output objects"},
		"unknown path": {func(value *Definition) {
			adapter := value.ConversationAdapters["conversations"]
			adapter.SubjectEvidence[0].SubjectPath = "receipt.unknown"
			value.ConversationAdapters["conversations"] = adapter
		}, "declared non-null output string"},
		"nonstring": {func(value *Definition) {
			conversationSubjectOutput(value)["type"] = "integer"
		}, "declared non-null output string"},
		"nullable string": {func(value *Definition) {
			conversationSubjectOutput(value)["type"] = []interface{}{"string", "null"}
		}, "declared non-null output string"},
		"nullable reference overrides type": {func(value *Definition) {
			action := value.Actions["send-message"]
			action.OutputSchema["$schema"] = "http://json-schema.org/draft-07/schema#"
			action.OutputSchema["definitions"] = map[string]interface{}{
				"nullableSubject": map[string]interface{}{"type": []interface{}{"string", "null"}},
			}
			conversationSubjectOutput(value)["$ref"] = "#/definitions/nullableSubject"
		}, "declared non-null output string"},
		"nullable parent": {func(value *Definition) {
			action := value.Actions["send-message"]
			properties := action.OutputSchema["properties"].(map[string]interface{})
			properties["receipt"].(map[string]interface{})["type"] = []interface{}{"object", "null"}
		}, "declared output objects"},
		"array traversal": {func(value *Definition) {
			action := value.Actions["send-message"]
			properties := action.OutputSchema["properties"].(map[string]interface{})
			properties["receipt"].(map[string]interface{})["type"] = "array"
		}, "declared output objects"},
		"reference overrides root object": {func(value *Definition) {
			action := value.Actions["send-message"]
			action.OutputSchema["$schema"] = "http://json-schema.org/draft-07/schema#"
			action.OutputSchema["$ref"] = "#/definitions/anything"
			action.OutputSchema["definitions"] = map[string]interface{}{"anything": map[string]interface{}{}}
		}, "declared output objects"},
		"undeclared credential": {func(value *Definition) {
			action := value.Actions["send-message"]
			action.Credentials[0].Name = "OTHER"
			value.Actions["send-message"] = action
		}, "not declared identically"},
		"credential kind": {func(value *Definition) {
			action := value.Actions["send-message"]
			action.Credentials[0].Kind = "other-oauth"
			value.Actions["send-message"] = action
		}, "not declared identically"},
		"credential optionality": {func(value *Definition) {
			action := value.Actions["send-message"]
			action.Credentials[0].Optional = true
			value.Actions["send-message"] = action
		}, "not declared identically"},
		"credential OAuth scopes": {func(value *Definition) {
			action := value.Actions["send-message"]
			action.Credentials[0].OAuth2.Scopes = []string{"chat:write"}
			value.Actions["send-message"] = action
		}, "not declared identically"},
		"noncanonical declaration": {func(value *Definition) {
			adapter := value.ConversationAdapters["conversations"]
			adapter.SubjectEvidence[0].SubjectPath += " "
			value.ConversationAdapters["conversations"] = adapter
		}, "subject evidence is invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			definition := cloneDefinition(conversationSubjectEvidenceSkill())
			test.mutate(definition)
			if err := validateDefinition(definition); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid subject evidence accepted: %v (want %q)", err, test.want)
			}
		})
	}
}

func TestConversationSubjectEvidenceSurvivesDefinitionStorageAndCopies(t *testing.T) {
	definition := conversationSubjectEvidenceSkill()
	copy := cloneDefinition(definition)
	if !reflect.DeepEqual(copy, definition) {
		t.Fatal("definition copy changed subject evidence")
	}
	adapter := copy.ConversationAdapters["conversations"]
	adapter.SubjectEvidence[0].SubjectPath = "mutated"
	copy.ConversationAdapters["conversations"] = adapter
	if definition.ConversationAdapters["conversations"].SubjectEvidence[0].SubjectPath != "receipt.externalConversationId" {
		t.Fatal("copied subject evidence shares its original slice")
	}
	payload, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	var restored Definition
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	if err := validateDefinition(&restored); err != nil {
		t.Fatalf("stored subject evidence is invalid: %v", err)
	}
	if !reflect.DeepEqual(restored.ConversationAdapters, definition.ConversationAdapters) {
		t.Fatal("stored subject evidence changed")
	}
}

func conversationSubjectEvidenceSkill() *Definition {
	definition := conversationAdapterOnlySkill()
	adapter := definition.ConversationAdapters["conversations"]
	adapter.SubjectEvidence = []ConversationSubjectEvidence{{Action: "send-message", SubjectPath: "receipt.externalConversationId"}}
	definition.ConversationAdapters["conversations"] = adapter
	definition.Actions["send-message"] = Action{
		Name: "send-message", Description: "Send a message", Risk: RiskLevelExternal, SideEffect: SideEffectExternal,
		Idempotency: IdempotencyRequired, Retry: ActionRetryPolicy{MaxAttempts: 1},
		Transport:   &TransportReference{Kind: "tool", Endpoint: "send-message"},
		Credentials: adapter.Credentials,
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"receipt": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
				"externalConversationId": map[string]interface{}{"type": "string"},
			}},
		}},
	}
	return definition
}

func conversationSubjectOutput(value *Definition) map[string]interface{} {
	action := value.Actions["send-message"]
	properties := action.OutputSchema["properties"].(map[string]interface{})
	receipt := properties["receipt"].(map[string]interface{})
	return receipt["properties"].(map[string]interface{})["externalConversationId"].(map[string]interface{})
}
