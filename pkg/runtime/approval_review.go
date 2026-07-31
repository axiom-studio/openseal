package runtime

import (
	"errors"
	"strings"
)

const (
	maxApprovalReviewSummary      = 2_000
	maxApprovalReviewTarget       = 4_000
	maxApprovalReviewContent      = 20_000
	maxApprovalReviewConsequences = 16
	maxApprovalReviewFacts        = 32
)

func validateApprovalReviewContext(review *ApprovalReviewContext) error {
	if review == nil {
		return nil
	}
	if strings.TrimSpace(review.Summary) == "" {
		return errors.New("summary is required")
	}
	if len(review.Summary) > maxApprovalReviewSummary ||
		len(review.Target) > maxApprovalReviewTarget ||
		len(review.Audience) > maxApprovalReviewSummary ||
		len(review.Purpose) > maxApprovalReviewSummary ||
		len(review.Content) > maxApprovalReviewContent {
		return errors.New("text exceeds the review context limit")
	}
	if len(review.Consequences) > maxApprovalReviewConsequences || len(review.Facts) > maxApprovalReviewFacts {
		return errors.New("too many review context items")
	}
	for _, consequence := range review.Consequences {
		if strings.TrimSpace(consequence) == "" || len(consequence) > maxApprovalReviewSummary {
			return errors.New("consequence must be non-empty and bounded")
		}
	}
	for _, fact := range review.Facts {
		if strings.TrimSpace(fact.Label) == "" || strings.TrimSpace(fact.Value) == "" ||
			len(fact.Label) > 200 || len(fact.Value) > maxApprovalReviewTarget {
			return errors.New("review fact label and value must be non-empty and bounded")
		}
	}
	return nil
}

func cloneApprovalReviewContext(review *ApprovalReviewContext) *ApprovalReviewContext {
	if review == nil {
		return nil
	}
	cloned := *review
	cloned.Consequences = append([]string(nil), review.Consequences...)
	cloned.Facts = append([]ApprovalReviewFact(nil), review.Facts...)
	return &cloned
}

func approvalReviewContextMap(review *ApprovalReviewContext) map[string]interface{} {
	if review == nil {
		return nil
	}
	result := map[string]interface{}{"summary": review.Summary}
	for key, value := range map[string]string{
		"target": review.Target, "audience": review.Audience, "content": review.Content, "purpose": review.Purpose,
	} {
		if strings.TrimSpace(value) != "" {
			result[key] = value
		}
	}
	if len(review.Consequences) > 0 {
		result["consequences"] = append([]string(nil), review.Consequences...)
	}
	if len(review.Facts) > 0 {
		facts := make([]interface{}, 0, len(review.Facts))
		for _, fact := range review.Facts {
			facts = append(facts, map[string]interface{}{"label": fact.Label, "value": fact.Value})
		}
		result["facts"] = facts
	}
	return result
}

func approvalReviewContextJSONSchema() map[string]interface{} {
	text := func(maxLength int) map[string]interface{} {
		return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxLength}
	}
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"description": "Operator-facing facts required to judge the proposed action. Include exact externally visible content and destination when applicable; never include authentication material.",
		"properties": map[string]interface{}{
			"summary":  withDescription(text(maxApprovalReviewSummary), "Plain-language statement of the exact effect being authorized."),
			"target":   withDescription(text(maxApprovalReviewTarget), "Stable human-readable destination or affected resource."),
			"audience": withDescription(text(maxApprovalReviewSummary), "People or system that will receive or observe the effect."),
			"content":  withDescription(text(maxApprovalReviewContent), "Exact externally visible text or payload that will be published or sent."),
			"purpose":  withDescription(text(maxApprovalReviewSummary), "Why this effect advances the Run objective."),
			"consequences": map[string]interface{}{
				"type": "array", "maxItems": maxApprovalReviewConsequences, "items": text(maxApprovalReviewSummary),
			},
			"facts": map[string]interface{}{
				"type": "array", "maxItems": maxApprovalReviewFacts,
				"items": map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{"label": text(200), "value": text(maxApprovalReviewTarget)},
					"required":   []string{"label", "value"},
				},
			},
		},
		"required": []string{"summary"},
	}
}

func withDescription(schema map[string]interface{}, description string) map[string]interface{} {
	schema["description"] = description
	return schema
}
