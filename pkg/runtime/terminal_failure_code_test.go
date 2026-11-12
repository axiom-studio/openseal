package runtime

import (
	"strings"
	"testing"
)

func TestTerminalFailureReplyUsesOnlyFiniteClassifiedFacts(t *testing.T) {
	for _, code := range []string{"cap_lookup_not_available", "cap_lookup_duplicate", "cap_lookup_already_loaded", "tool_call_cardinality", "tool_call_identity_missing", "tool_call_not_authorized", "provider_authentication_failed", "provider_quota_exhausted", "workspace_operation_failed", "action_admission_failed", "unknown token=PRIVATE"} {
		text := TerminalFailureReply(code)
		if len(text) > 512 || !strings.Contains(text, "I couldn't finish") || !strings.Contains(text, "Ask me to try again") || strings.Contains(text, "PRIVATE") {
			t.Fatalf("unbounded/unsafe failure reply: %q", text)
		}
	}
	if TerminalFailureReply("cap_lookup_not_available") == TerminalFailureReply("cap_lookup_already_loaded") {
		t.Fatal("distinct causes were falsely collapsed")
	}
}

func TestTerminalFailureReplyDistinguishesSafeValidationAndBudgetCauses(t *testing.T) {
	for _, tc := range []struct{ input, code, reason string }{
		{"provider_output_truncated", "provider_output_truncated", "stopped before completing"},
		{"capability_input_budget_exhausted", "budget_exhausted", "execution budget"},
		{"tool_arguments_invalid", "tool_arguments_invalid", "arguments that did not match"},
		{"action_review_invalid", "action_review_invalid", "operation review details"},
		{"action_external_identity_invalid", "action_external_identity_invalid", "identification for an external operation"},
		{"final_skill_identity_invalid", "final_skill_identity_invalid", "Skill reference"},
		{"conversation_task_review_invalid", "conversation_task_review_invalid", "invalid review response"},
		{"conversation_task_review_failed", "conversation_task_review_failed", "did not meet the review requirements"},
		{"provider_invalid_turn_outcome", "provider_invalid_response", "could not be safely executed"},
		{"tool_arguments_invalid: token=PRIVATE", "execution_failed", "execution failure"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if terminalFailureCode(tc.input) != tc.code {
				t.Fatal("lost the finite terminal cause")
			}
			text := TerminalFailureReply(tc.input)
			if !strings.Contains(text, tc.reason) || !strings.Contains(text, "Ask me to try again") || len(text) > 512 || strings.Contains(text, "PRIVATE") {
				t.Fatal("failure reply lost safe specificity or leaked arbitrary code text")
			}
		})
	}
	if TerminalFailureReply("provider_output_truncated") == TerminalFailureReply("provider_invalid_response") {
		t.Fatal("truncated output was collapsed into malformed output")
	}
}
