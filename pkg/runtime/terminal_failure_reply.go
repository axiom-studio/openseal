package runtime

import (
	"strings"

	"github.com/axiom-studio/openseal/pkg/skillerror"
)

// TerminalFailureReply renders only kernel-classified facts. Provider messages,
// tool arguments, identifiers and command output are never public reply text.
// A failed attempt does not authorize a correction or another model exchange.
func TerminalFailureReply(code string) string {
	reason := "this attempt encountered an execution failure"
	switch terminalFailureCode(code) {
	case "cap_lookup_not_available":
		reason = "the model requested a capability outside the available set"
	case "cap_lookup_duplicate":
		reason = "the model repeated a capability in the same lookup"
	case "cap_lookup_already_loaded":
		reason = "the model tried to load a capability that was already available"
	case "tool_call_cardinality":
		reason = "the model combined operations that must run separately"
	case "tool_call_identity_missing":
		reason = "the model returned an operation without a valid call identity"
	case "tool_call_not_authorized":
		reason = "the model requested an operation that was not offered for this attempt"
	case "provider_invalid_response":
		reason = "the model returned a response that could not be safely executed"
	case "provider_continuation_failed":
		reason = "the model provider did not supply valid continuation data"
	case "provider_authentication_failed":
		reason = "the model provider rejected the configured credential"
	case "provider_access_denied":
		reason = "the model provider denied access to the configured model"
	case "provider_quota_exhausted":
		reason = "the model provider's quota is exhausted"
	case "workspace_credits_exhausted":
		reason = "this workspace has no model credits available"
	case "workspace_daily_limit_reached":
		reason = "this workspace reached its daily model credit limit"
	case "provider_rate_limited":
		reason = "the model provider temporarily limited requests"
	case "provider_unavailable":
		reason = "the model provider was unavailable"
	case "provider_request_rejected":
		reason = "the model provider rejected the request"
	case "model_configuration_failed":
		reason = "the configured model connection was unavailable or incomplete"
	case "budget_exhausted":
		reason = "this attempt reached its execution budget"
	case "workspace_operation_failed":
		reason = "a Workspace operation failed"
	case "action_admission_failed":
		reason = "the proposed operation could not pass validation or authorization"
	case "action_failed":
		reason = "an operation failed"
	case "dependency_failed":
		reason = "work this request depended on failed"
	case "task_completion_rejected":
		reason = "the result did not meet the task's completion requirements after a revision"
	}
	return "I couldn't finish this request because " + reason + ". I stopped this attempt. Ask me to try again when you're ready."
}

// Normalize a bounded vocabulary rather than forwarding a host-supplied code.
func terminalFailureCode(code string) string {
	switch strings.TrimSpace(code) {
	case "cap_lookup_not_available", "cap_lookup_duplicate", "cap_lookup_already_loaded", "tool_call_cardinality", "tool_call_identity_missing", "tool_call_not_authorized":
		return strings.TrimSpace(code)
	case "provider_invalid_turn_outcome", "provider_invalid_response", "provider_output_truncated":
		return "provider_invalid_response"
	case "provider_tool_continuation_missing", "provider_opaque_context_rejected", "provider_referenced_context_rejected", "provider_continuation_failed":
		return "provider_continuation_failed"
	case "provider_authentication_failed", "provider_access_denied", "provider_quota_exhausted", "provider_rate_limited", "provider_request_rejected":
		return strings.TrimSpace(code)
	case "provider_unavailable", "provider_model_unavailable":
		return "provider_unavailable"
	case "gateway_credits_exhausted", "workspace_credits_exhausted":
		return "workspace_credits_exhausted"
	case "gateway_daily_limit_reached", "workspace_daily_limit_reached":
		return "workspace_daily_limit_reached"
	case "model_credential_unavailable", "requires_model_credential", "model_configuration_failed":
		return "model_configuration_failed"
	case "turn_input_budget_exhausted", "turn_output_budget_exhausted", "workspace_turn_budget_exhausted", "workspace_action_budget_exhausted", "workspace_operation_limit", "budget_exhausted":
		return "budget_exhausted"
	case "workspace_operation_failed", "workspace_host_unavailable":
		return "workspace_operation_failed"
	case "action_admission_failed", "action_failed", "dependency_failed", "task_completion_rejected":
		return strings.TrimSpace(code)
	default:
		return "execution_failed"
	}
}

func terminalFailureCodeFromCheckpoint(checkpoint map[string]interface{}) string {
	// Atlas writes this bounded marker only after the one allowed completion
	// repair fails. Its protected review checkpoint is never model-authored.
	// Prefer that terminal cause over earlier, contributory action failures.
	if review, ok := checkpoint["_atlasTaskCompletionReview"].(map[string]interface{}); ok &&
		review["phase"] == "review" && review["failureCode"] == "task_completion_rejected" &&
		terminalFailureCounterIsOne(review["version"]) && terminalFailureCounterIsOne(review["repairs"]) {
		return "task_completion_rejected"
	}
	if failure, ok := checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{}); ok {
		if code, ok := failure["code"].(string); ok && code != "" {
			classified := terminalFailureCode(code)
			if classified == "task_completion_rejected" {
				return "execution_failed" // This cause requires the protected review marker above.
			}
			return classified
		}
		switch failure["kind"] {
		case "workspace":
			return "workspace_operation_failed"
		case "proposal":
			return "action_admission_failed"
		case "action":
			return "action_failed"
		case "dependency", "delegation":
			return "dependency_failed"
		}
	}
	if _, active := ReadToolFeedbackCorrection(checkpoint); active {
		return "action_failed"
	}
	return "execution_failed"
}

func terminalFailureReplyFromCheckpoint(checkpoint map[string]interface{}) string {
	code := terminalFailureCodeFromCheckpoint(checkpoint)
	reply := TerminalFailureReply(code)
	if code != "task_completion_rejected" {
		return reply
	}
	// Action history is kernel-owned execution evidence. Report a bounded
	// contributing cause without presenting it as the completion review's
	// terminal verdict, or copying arbitrary tool output into a public reply.
	for _, entry := range actionHistoryEntries(checkpoint) {
		failed := entry["status"] == ActionCallStatusFailed || entry["status"] == string(ActionCallStatusFailed)
		errorCode, _ := entry["errorCode"].(string)
		raw, _ := entry["errorDetails"].(map[string]interface{})
		if !failed || errorCode == "" || entry["failureKind"] != errorCode || raw["failureKind"] != errorCode || raw["retryable"] != "false" {
			continue
		}
		details := map[string]string{}
		for key, value := range raw {
			if text, ok := value.(string); ok {
				details[key] = text
			}
		}
		if failure := skillerror.NewActionError(errorCode, "", details); failure != nil && failure.HasRateLimitedSource() {
			return reply + " Some sources rate-limited requests (HTTP 429). Try those sources again later."
		}
	}
	return reply
}

// Accept the in-memory and persisted JSON number forms without coercing
// strings, booleans, fractional values or arbitrary host-supplied objects.
func terminalFailureCounterIsOne(value interface{}) bool {
	switch value := value.(type) {
	case int:
		return value == 1
	case int64:
		return value == 1
	case float64:
		return value == 1
	default:
		return false
	}
}
