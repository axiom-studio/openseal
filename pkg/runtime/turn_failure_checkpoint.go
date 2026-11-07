package runtime

// This file is the old per-Turn failure checkpoint path. Nothing in the run
// loop reads or writes it: loop Runs carry AgentRun.FailureExplanation
// (final_failure_explanation.go). It stays only while its old callers exist
// (tool_feedback_correction.go, action_history.go, action_failure_evidence.go: del-1b;
// source_access_challenge.go: del-1a; conversation_run.go: CONV-07;
// catalog_turn_resolver.go, agent_run_worker.go, dependency.go,
// collaboration.go). Whoever deletes the last caller deletes this file.

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skillerror"
)

// FinalFailureExplanationCheckpointKey is written only by the kernel after an
// action or proposal fails. Its next hosted turn can explain the failure but
// cannot execute or schedule more work. A new explicit user attempt starts with
// a fresh checkpoint; it does not inherit this terminal boundary.
const FinalFailureExplanationCheckpointKey = "_opensealFinalFailureExplanation"

func requiresFinalFailureExplanation(checkpoint map[string]interface{}) bool {
	_, ok := checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	return ok
}

func checkpointFinalFailureExplanation(checkpoint map[string]interface{}, kind, message string) map[string]interface{} {
	result := deepCloneCheckpointMap(checkpoint)
	if result == nil {
		result = map[string]interface{}{}
	}
	if !requiresFinalFailureExplanation(result) {
		result[FinalFailureExplanationCheckpointKey] = map[string]interface{}{
			"kind": strings.TrimSpace(kind), "message": strings.TrimSpace(message),
		}
	}
	return result
}

// FinalFailureExplanationSystemInstruction defines the generic hosted form contract.
// A host using a text-only explanation subcall can replace this exact instruction.
const FinalFailureExplanationSystemInstruction = "This attempt has stopped after the kernel-recorded failure in continuationCheckpoint._opensealFinalFailureExplanation. Give one brief, useful final reply in your configured voice, explaining what did not work and any next step the user actually needs. Ground the explanation in the recorded failure and succeeded evidence. Do not expose internal tool names, IDs, or governance details; do not guess that the user's billing plan, account, or permissions are wrong without evidence. Do not attempt, retry, repair, request setup, schedule, wait, fork, or delegate further work. Return nextRunStatus completed, no wakeCondition or proposals, and your final reply in runOutput.summary. Earlier instructions to continue work no longer apply to this failed attempt."

func checkpointTerminalFailure(checkpoint map[string]interface{}, code string) map[string]interface{} {
	result := checkpointFinalFailureExplanation(checkpoint, "execution", "The requested work did not complete.")
	failure := result[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	failure["code"] = terminalFailureCode(code)
	return result
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
	if code == "action_failed" {
		if cause := checkpointFailedActionCause(checkpoint); cause != "" {
			return "I couldn't finish this request because " + cause + ". I stopped this attempt. Ask me to try again when you're ready."
		}
	}
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

// checkpointFailedActionCause names the tool whose action failed last and what it
// answered, for the kernel's fallback reply when no model could explain the
// failure. Transport wrappers around the answer are dropped; credentials were
// already removed at the ActionWorker boundary.
func checkpointFailedActionCause(checkpoint map[string]interface{}) string {
	last, _ := checkpoint["lastAction"].(map[string]interface{})
	if last == nil || fmt.Sprint(last["status"]) != string(ActionCallStatusFailed) || last["errorCode"] != nil {
		// A typed failure has its own exact wording.
		return ""
	}
	message, _ := last["error"].(string)
	// Only the failure that stopped this attempt is its cause, never an
	// earlier one the model already handled.
	if recorded := recordedToolFailure(checkpoint); recorded == "" || recorded != toolFeedbackMessage(message) {
		return ""
	}
	message = failedActionAnswer(message)
	if message == "" {
		return ""
	}
	label := failedActionToolLabel(fmt.Sprint(last["skillId"]))
	if label == "" {
		return "a tool reported: " + message
	}
	return label + " reported: " + message
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
