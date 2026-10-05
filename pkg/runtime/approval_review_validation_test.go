package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestApprovalReviewValidationFailureHasTypedCauseWithoutChangingContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		review *ApprovalReviewContext
		cause  string
	}{
		{"optional_nil", nil, ""},
		{"valid", &ApprovalReviewContext{Summary: "Publish PRIVATE", Consequences: []string{"Notify recipient"}, Facts: []ApprovalReviewFact{{Label: "Target", Value: "PRIVATE"}}}, ""},
		{"summary", &ApprovalReviewContext{Summary: " "}, "summary is required"},
		{"text_limit", &ApprovalReviewContext{Summary: strings.Repeat("x", maxApprovalReviewSummary+1)}, "text exceeds the review context limit"},
		{"item_limit", &ApprovalReviewContext{Summary: "Publish", Facts: make([]ApprovalReviewFact, maxApprovalReviewFacts+1)}, "too many review context items"},
		{"consequence", &ApprovalReviewContext{Summary: "Publish", Consequences: []string{" "}}, "consequence must be non-empty and bounded"},
		{"fact", &ApprovalReviewContext{Summary: "Publish", Facts: []ApprovalReviewFact{{Label: "Target", Value: " "}}}, "review fact label and value must be non-empty and bounded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.review)
			err := validateApprovalReviewContext(tc.review)
			if tc.cause == "" {
				if err != nil {
					t.Fatal("valid review no longer accepted")
				}
			} else {
				var typed *approvalReviewValidationError
				if !errors.Is(err, ErrInvalidApprovalReviewContext) || !errors.As(err, &typed) || err.Error() != tc.cause || !errors.Is(err, typed.cause) {
					t.Fatal("review rejection lost its exact original cause or typed identity")
				}
			}
			after, _ := json.Marshal(tc.review)
			if string(before) != string(after) {
				t.Fatal("review validation mutated caller-owned data")
			}
		})
	}
	// The real compiler adds context to the same canonical cause; consumers must
	// still identify it without reading either review values or error strings.
	_, err := CompileHostedTurnForm(HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, NextRunStatus: AgentRunStatusRunning,
		ProposedAction: &HostedTurnActionForm{Capability: "records.publish", Summary: "Publish", IdempotencyKey: "key", Arguments: map[string]interface{}{}, ReviewContext: &ApprovalReviewContext{Summary: " "}},
	}, []capability.ModelAction{{Name: "records.publish", SideEffect: capability.SideEffectExternal}})
	if !errors.Is(err, ErrInvalidApprovalReviewContext) {
		t.Fatal("compiler discarded the canonical review rejection identity")
	}
}
