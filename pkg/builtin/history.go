package builtin

import "github.com/axiom-studio/openseal/pkg/skill"

func HistoryAction() skill.Action {
	return skill.Action{
		Name:        ReadConversationHistory,
		Description: "Retrieve visible original messages from this run's current conversation. Page backwards using nextBeforeSequence, or read the rest of one original using messageId and nextOffsetBytes. Cannot access other conversations.",
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"messageId":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128},
			"beforeSequence": map[string]interface{}{"type": "integer", "minimum": 0},
			"offsetBytes":    map[string]interface{}{"type": "integer", "minimum": 0},
			"limit":          map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 10},
		}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"messages"}, "properties": map[string]interface{}{
			"messages": map[string]interface{}{"type": "array", "maxItems": 10, "items": map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"id", "sequence", "sender", "content", "offsetBytes", "createdAt", "intent"}, "properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string"}, "sequence": map[string]interface{}{"type": "integer", "minimum": 1},
				"sender":      map[string]interface{}{"type": "object", "required": []interface{}{"type", "id"}, "properties": map[string]interface{}{"type": map[string]interface{}{"type": "string"}, "id": map[string]interface{}{"type": "string"}}},
				"content":     map[string]interface{}{"type": "string", "maxLength": 4096},
				"offsetBytes": map[string]interface{}{"type": "integer", "minimum": 0}, "nextOffsetBytes": map[string]interface{}{"type": "integer", "minimum": 1},
				"createdAt": map[string]interface{}{"type": "string"}, "intent": map[string]interface{}{"type": "string"}, "replyToMessageId": map[string]interface{}{"type": "string"},
				"references": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
			}}},
			"nextBeforeSequence": map[string]interface{}{"type": "integer", "minimum": 1},
		}},
		SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
		Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
	}
}
