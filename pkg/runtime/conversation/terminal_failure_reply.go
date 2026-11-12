package conversation

import (
	"context"
	"maps"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skillerror"
)

// TerminalFailureReply renders only kernel-classified facts. Provider messages,
// tool arguments, identifiers and command output are never public reply text.
// A failed attempt does not authorize a correction or another model exchange.
func TerminalFailureReply(code string) string {
	reason := "this attempt encountered an execution failure"
	switch terminalFailureCode(code) {
	case "model_image_input_unsupported":
		return "I couldn't finish this because my model only reads text, and this request included an image from the chat. Choose a model that can read images in my settings, then ask me again."
	case "agent_behavior_fixed":
		return "I can't change my own instructions or behavior. This Agent's setup is fixed. I can still help with your request as I am; to use custom instructions, create your own Agent with them."
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
	case "provider_output_truncated":
		reason = "the model provider stopped before completing its response"
	case "tool_arguments_invalid":
		reason = "the model supplied operation arguments that did not match the required format"
	case "action_review_invalid":
		reason = "the model supplied missing or invalid operation review details"
	case "action_external_identity_invalid":
		reason = "the model supplied missing or invalid identification for an external operation"
	case "final_skill_identity_invalid":
		reason = "the model's final answer cited an unavailable or repeated Skill reference"
	case "conversation_task_review_invalid":
		reason = "the task reviewer returned an invalid review response"
	case "conversation_task_review_failed":
		reason = "the task result did not meet the review requirements"
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
	case "provider_output_truncated", "tool_arguments_invalid", "action_review_invalid", "action_external_identity_invalid", "final_skill_identity_invalid", "conversation_task_review_invalid", "conversation_task_review_failed":
		return strings.TrimSpace(code)
	case "provider_invalid_turn_outcome", "provider_invalid_response":
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
	case "capability_input_budget_exhausted", "turn_input_budget_exhausted", "turn_output_budget_exhausted", "workspace_turn_budget_exhausted", "workspace_action_budget_exhausted", "workspace_operation_limit", "budget_exhausted":
		return "budget_exhausted"
	case "workspace_operation_failed", "workspace_host_unavailable":
		return "workspace_operation_failed"
	case "action_admission_failed", "action_failed", "dependency_failed", "task_completion_rejected", "agent_behavior_fixed", "model_image_input_unsupported":
		return strings.TrimSpace(code)
	default:
		return "execution_failed"
	}
}

// terminalFailureCodeOf is the public reply class of a recorded failure. A
// failure without a code is classed by its SPEC §2.2 kind.
func terminalFailureCodeOf(f *models.RunFailureExplanation) string {
	if f == nil {
		return "execution_failed"
	}
	if strings.TrimSpace(f.Code) != "" {
		return terminalFailureCode(f.Code)
	}
	if f.Kind == RunFailureKindWorkspace {
		return "workspace_operation_failed"
	}
	return "execution_failed"
}

// terminalFailureReplyOf is the kernel's reply for a recorded failure.
// stoppingCall is the failed governed call that stopped the attempt, and
// latestFailedCall the Run's newest failed governed call; either may be nil.
func terminalFailureReplyOf(f *models.RunFailureExplanation, stoppingCall, latestFailedCall *models.ActionCall) string {
	code := terminalFailureCodeOf(f)
	reply := TerminalFailureReply(code)
	switch code {
	case "action_failed":
		if cause := failedActionCause(stoppingCall); cause != "" {
			return "I couldn't finish this request because " + cause + ". I stopped this attempt. Ask me to try again when you're ready."
		}
	case "task_completion_rejected":
		// Report a bounded contributing cause without presenting it as the
		// completion review's verdict or copying tool output into the reply.
		if failure := typedActionFailure(latestFailedCall); failure != nil && failure.HasRateLimitedSource() {
			return reply + " Some sources rate-limited requests (HTTP 429). Try those sources again later."
		}
	}
	return reply
}

// TerminalRunFailureReply is the kernel's reply for a failed Run. A failed
// governed call in the Run's last step with tool entries is what stopped the
// attempt, and gives the typed rate-limit, browser and file-size replies. A
// failure the model already moved past (later steps ran tools) is not named.
// A store that cannot be read only loses that detail, never the reply.
func TerminalRunFailureReply(ctx context.Context, sessions LoopSessionEntryLister, actions models.ActionCallReader, run *models.AgentRun) string {
	if run == nil {
		return TerminalFailureReply("execution_failed")
	}
	code := terminalFailureCodeOf(run.FailureExplanation)
	var stopping, latest *models.ActionCall
	if run.Status == models.AgentRunStatusFailed && (code == "action_failed" || code == "execution_failed") {
		stopping = stoppingFailedRunActionCall(ctx, sessions, actions, run)
		if failure := typedActionFailure(stopping); failure != nil {
			if failure.HasRateLimitedSource() {
				return "I couldn't finish this request because one or more sources rate-limited requests (HTTP 429). I stopped this attempt. Try again later."
			}
			switch failure.Code() {
			case "browser_proxy_authentication_failed":
				return "I couldn't finish this request because the platform's browser connection was rejected. I stopped this attempt. Try again later."
			case "browser_proxy_unavailable":
				return "I couldn't finish this request because the platform's browser connection was unavailable. I stopped this attempt. Try again later."
			case "file_too_large":
				return fileTooLargeReply(failure.Details())
			}
		}
	}
	if code == "task_completion_rejected" {
		latest = latestFailedRunActionCall(ctx, sessions, actions, run)
	}
	return terminalFailureReplyOf(run.FailureExplanation, stopping, latest)
}

const latestFailedCallScanLimit = 50

// latestFailedRunActionCall is the call of the Run's newest error tool entry
// that carries an action call, when that call is this Run's and failed.
func latestFailedRunActionCall(ctx context.Context, sessions LoopSessionEntryLister, actions models.ActionCallReader, run *models.AgentRun) *models.ActionCall {
	if sessions == nil || actions == nil || run.ID == "" {
		return nil
	}
	entries, err := sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, Types: []models.SessionEntryType{models.SessionEntryTool},
		States: []models.ToolEntryState{models.ToolEntryError}, Descending: true, Limit: latestFailedCallScanLimit,
	})
	if err != nil {
		return nil
	}
	return firstFailedRunActionCall(ctx, actions, run, entries, "")
}

// stoppingFailedRunActionCall is like latestFailedRunActionCall, but only
// looks at the Run's last step with tool entries: a failure in an earlier
// step was followed by more work, so it did not stop the attempt.
func stoppingFailedRunActionCall(ctx context.Context, sessions LoopSessionEntryLister, actions models.ActionCallReader, run *models.AgentRun) *models.ActionCall {
	if sessions == nil || actions == nil || run.ID == "" {
		return nil
	}
	entries, err := sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, Types: []models.SessionEntryType{models.SessionEntryTool},
		Descending: true, Limit: latestFailedCallScanLimit,
	})
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry != nil && entry.RunID == run.ID {
			if entry.StepID == "" {
				return nil
			}
			return firstFailedRunActionCall(ctx, actions, run, entries, entry.StepID)
		}
	}
	return nil
}

// firstFailedRunActionCall walks entries newest first (stopping at the first
// entry of another step when stepID is set) to the first error entry with an
// action call, and returns that call if it is this Run's and failed.
func firstFailedRunActionCall(ctx context.Context, actions models.ActionCallReader, run *models.AgentRun, entries []*models.SessionEntry, stepID string) *models.ActionCall {
	for _, entry := range entries {
		if entry == nil || entry.RunID != run.ID {
			continue
		}
		if stepID != "" && entry.StepID != stepID {
			return nil
		}
		if entry.State != models.ToolEntryError || entry.ActionCallID == "" {
			continue
		}
		call, err := actions.GetActionCall(ctx, run.Scope, entry.ActionCallID)
		if err != nil || call == nil || call.Scope != run.Scope || call.RunID != run.ID {
			return nil
		}
		if call.Status != models.ActionCallStatusFailed {
			return nil
		}
		return call
	}
	return nil
}

// typedActionFailure is the call's typed failure when its stored error is
// exactly the sanitized text of that type; model or tool text never is.
func typedActionFailure(call *models.ActionCall) *skillerror.ActionError {
	if call == nil || call.Status != models.ActionCallStatusFailed || call.ErrorCode == "" {
		return nil
	}
	failure := skillerror.NewActionError(call.ErrorCode, "", call.ErrorDetails)
	if failure == nil || failure.Code() != call.ErrorCode || call.Error != failure.Error() || !maps.Equal(failure.Details(), call.ErrorDetails) {
		return nil
	}
	return failure
}

// fileTooLargeReply says how large the file was and how large a file an
// action can be given, from the failure's bounded sizes.
func fileTooLargeReply(details map[string]string) string {
	size, limit := details["fileMegabytes"], details["limitMegabytes"]
	if size == "" || limit == "" {
		return "I couldn't finish this because the file is too large to hand to another app. Make it smaller, then ask me again."
	}
	return "I couldn't finish this because the file is " + size + " MB, and I can hand files of up to " + limit + " MB to another app. Make it smaller, for example by compressing it or splitting it into parts, then ask me again."
}

const maximumFailedActionCauseBytes = 300

// failedActionCause names the tool whose action failed and what it answered,
// for the kernel's reply when no model explained the failure. Transport
// wrappers around the answer are dropped; credentials were already removed at
// the action.Worker boundary.
func failedActionCause(call *models.ActionCall) string {
	if call == nil || call.Status != models.ActionCallStatusFailed || call.ErrorCode != "" {
		// A typed failure has its own exact wording.
		return ""
	}
	message := failedActionAnswer(call.Error)
	if message == "" {
		return ""
	}
	label := failedActionToolLabel(call.SkillID)
	if label == "" {
		return "a tool reported: " + message
	}
	return label + " reported: " + message
}

func failedActionAnswer(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	for {
		trimmed := message
		if field, rest, ok := strings.Cut(trimmed, " "); ok && strings.Contains(field, "=") && !strings.Contains(field, ":") {
			trimmed = rest // a leading diagnostic key=value, such as runId=…
		}
		lower := strings.ToLower(trimmed)
		for _, wrapper := range []string{"atlas error:", "execution:", "execution failed:", "action failed:", "skill error:", "error:"} {
			if strings.HasPrefix(lower, wrapper) {
				trimmed = strings.TrimSpace(trimmed[len(wrapper):])
				break
			}
		}
		if trimmed == message {
			break
		}
		message = trimmed
	}
	message = strings.TrimRight(message, ". ")
	if len(message) > maximumFailedActionCauseBytes {
		cut := maximumFailedActionCauseBytes
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "…"
	}
	return message
}

// failedActionToolLabel turns a Skill ID such as skill-slack or
// openseal.browser into a plain tool name, Slack or Browser.
func failedActionToolLabel(skillID string) string {
	skillID = strings.TrimSpace(skillID)
	if skillID == "" || skillID == "<nil>" {
		return ""
	}
	if index := strings.LastIndexAny(skillID, "./"); index >= 0 {
		skillID = skillID[index+1:]
	}
	skillID = strings.TrimPrefix(skillID, "skill-")
	words := strings.FieldsFunc(skillID, func(r rune) bool { return r == '-' || r == '_' })
	for index, word := range words {
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		words[index] = string(runes)
	}
	return strings.Join(words, " ")
}
