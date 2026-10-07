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

// ErrInvalidApprovalReviewContext identifies a canonical review validation
// failure without exposing review content or requiring consumers to parse text.
var ErrInvalidApprovalReviewContext = errors.New("invalid approval review context")

type approvalReviewValidationError struct{ cause error }

func (e *approvalReviewValidationError) Error() string { return e.cause.Error() }
func (e *approvalReviewValidationError) Unwrap() []error {
	return []error{ErrInvalidApprovalReviewContext, e.cause}
}

func invalidApprovalReviewContext(message string) error {
	return &approvalReviewValidationError{cause: errors.New(message)}
}

func validateApprovalReviewContext(review *ApprovalReviewContext) error {
	if review == nil {
		return nil
	}
	if strings.TrimSpace(review.Summary) == "" {
		return invalidApprovalReviewContext("summary is required")
	}
	if len(review.Summary) > maxApprovalReviewSummary ||
		len(review.Target) > maxApprovalReviewTarget ||
		len(review.Audience) > maxApprovalReviewSummary ||
		len(review.Purpose) > maxApprovalReviewSummary ||
		len(review.Content) > maxApprovalReviewContent {
		return invalidApprovalReviewContext("text exceeds the review context limit")
	}
	if len(review.Consequences) > maxApprovalReviewConsequences || len(review.Facts) > maxApprovalReviewFacts {
		return invalidApprovalReviewContext("too many review context items")
	}
	for _, consequence := range review.Consequences {
		if strings.TrimSpace(consequence) == "" || len(consequence) > maxApprovalReviewSummary {
			return invalidApprovalReviewContext("consequence must be non-empty and bounded")
		}
	}
	for _, fact := range review.Facts {
		if strings.TrimSpace(fact.Label) == "" || strings.TrimSpace(fact.Value) == "" ||
			len(fact.Label) > 200 || len(fact.Value) > maxApprovalReviewTarget {
			return invalidApprovalReviewContext("review fact label and value must be non-empty and bounded")
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

// intentApprovalReviewContext names an action awaiting review from its own
// declared intent when the proposer supplied no review context, so a reviewer
// sees "Click 'Search'" rather than a generic summary. It reads only the
// secret-safe preview arguments; the target is the action's URL argument or
// its external operation resource.
func intentApprovalReviewContext(preview map[string]interface{}, operation *ExternalOperationIdentity) *ApprovalReviewContext {
	arguments, _ := preview["arguments"].(map[string]interface{})
	intent, _ := arguments["intent"].(string)
	intent = strings.Join(strings.Fields(intent), " ")
	if intent == "" {
		return nil
	}
	if runes := []rune(intent); len(runes) > 280 {
		intent = strings.TrimSpace(string(runes[:280])) + "…"
	}
	review := &ApprovalReviewContext{Summary: intent}
	if target, _ := arguments["url"].(string); strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") {
		review.Target = target
	} else if operation != nil {
		review.Target = strings.TrimSpace(operation.Resource)
	}
	return review
}
