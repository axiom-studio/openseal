package runtime

import (
	"errors"
	"strings"
)

// ToolFeedbackCorrectionCheckpointKey is kernel-owned state for corrections
// authored from an executed tool's feedback. It is not a transport retry policy.
const ToolFeedbackCorrectionCheckpointKey = "_opensealToolFeedbackCorrection"

// MaximumToolFeedbackCorrections is zero: an executed failure stops the attempt.
// Legacy checkpoint counters remain readable, but cannot authorize new work.
const MaximumToolFeedbackCorrections = 0

const maximumToolFeedbackMessageBytes = 4096

// ToolFeedbackCorrection is the bounded feedback visible to a hosted model.
// Counters are committed with action admission and cannot be changed by a model.
type ToolFeedbackCorrection struct {
	Kind                 string
	Message              string
	CorrectionsUsed      int
	CorrectionsRemaining int
	LastFailureID        string
}

func ReadToolFeedbackCorrection(checkpoint map[string]interface{}) (ToolFeedbackCorrection, bool) {
	state, ok := checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	if !ok {
		return ToolFeedbackCorrection{}, false
	}
	used := toolFeedbackInteger(state["correctionsUsed"])
	used = min(MaximumToolFeedbackCorrections, max(0, used))
	kind, _ := state["kind"].(string)
	message, _ := state["message"].(string)
	failureID, _ := state["lastFailureId"].(string)
	return ToolFeedbackCorrection{Kind: kind, Message: message, CorrectionsUsed: used,
		CorrectionsRemaining: MaximumToolFeedbackCorrections - used, LastFailureID: failureID}, true
}

func toolFeedbackInteger(value interface{}) int {
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func toolFeedbackMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "The requested action did not complete."
	}
	if len(message) > maximumToolFeedbackMessageBytes {
		message = message[:maximumToolFeedbackMessageBytes] + "…"
	}
	return message
}

func checkpointToolFeedbackFailure(checkpoint map[string]interface{}, call *ActionCall) map[string]interface{} {
	result := checkpointFinalFailureExplanation(checkpoint, "action", toolFeedbackMessage(call.Error))
	return checkpointTerminalFailure(result, "action_failed")
}

// admitToolFeedbackCorrection is called only after normal catalog, argument,
// policy and external-operation checks. Its result is persisted atomically with
// the new ActionCall and the Run revision. An idempotent proposal replay does
// not persist the candidate checkpoint or debit its allowance again.
func admitToolFeedbackCorrection(checkpoint map[string]interface{}, call *ActionCall) (map[string]interface{}, string) {
	result := deepCloneCheckpointMap(checkpoint)
	feedback, active := ReadToolFeedbackCorrection(result)
	if !active {
		return result, ""
	}
	state := result[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	if state["admittedActionCallId"] == call.ID {
		return result, ""
	}
	if requiresFinalFailureExplanation(result) || feedback.CorrectionsRemaining == 0 {
		return result, feedback.Message
	}
	digest := ComputeActionSemanticDigest(call)
	if digests, ok := state["failedSemanticDigests"].([]interface{}); ok {
		for _, prior := range digests {
			if digest != "" && prior == digest {
				return result, feedback.Message + " The unchanged failed request was not sent again."
			}
		}
	}
	state["correctionsUsed"] = feedback.CorrectionsUsed + 1
	state["admittedActionCallId"] = call.ID
	return result, ""
}

func checkpointToolFeedbackSuccess(checkpoint map[string]interface{}, call *ActionCall) map[string]interface{} {
	result := deepCloneCheckpointMap(checkpoint)
	state, ok := result[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	if ok && state["admittedActionCallId"] == call.ID {
		delete(result, ToolFeedbackCorrectionCheckpointKey)
	}
	return result
}

func checkpointToolFeedbackDenied(checkpoint map[string]interface{}, call *ActionCall) map[string]interface{} {
	message := toolFeedbackMessage(call.Error)
	if feedback, active := ReadToolFeedbackCorrection(checkpoint); active {
		message = feedback.Message + " The proposed correction was not authorized: " + message
	}
	return checkpointFinalFailureExplanation(checkpoint, "action", message)
}

func projectToolFeedbackCorrection(request *HostedTurnRequest) {
	if _, active := ReadToolFeedbackCorrection(request.ContinuationCheckpoint); !active {
		return
	}
	request.Actions = nil
	request.Workspace = nil
	request.WorkspaceOperations = nil
	request.WorkspaceCredentials = nil
	request.EligibleAgents = nil
	request.RunbookOperations = nil
	request.ConversationTasks = nil
	request.SystemInstructions = append(request.SystemInstructions,
		"A prior tool failed. This attempt has stopped; no correction or further operation is authorized. The kernel will deliver the failure reply without another model request.")
}

func validateToolFeedbackCorrectionOutcome(run *AgentRun, outcome *TurnOutcome) error {
	if _, active := ReadToolFeedbackCorrection(run.Checkpoint); !active || requiresFinalFailureExplanation(run.Checkpoint) {
		return nil
	}
	if outcome == nil || outcome.WakeCondition != nil || outcome.ProposedFork != nil || outcome.ProposedDelegation != nil || outcome.ProposedRunbook != nil {
		return errors.New("tool feedback corrections cannot schedule, fork, or delegate more attempts")
	}
	if outcome.NextRunStatus != AgentRunStatusCompleted || len(outcome.ProposedActions) != 0 {
		return errors.New("tool feedback requires one corrected action or a final explanation")
	}
	if silent, _ := outcome.RunOutput["silent"].(bool); silent {
		return errors.New("a tool failure explanation must be visible to the user")
	}
	if _, intake := outcome.RunOutput[AgentRequestDecisionOutputKey]; intake {
		return errors.New("a tool failure explanation cannot repeat an intake decision")
	}
	if strings.TrimSpace(outcome.OutputSummary) == "" && strings.TrimSpace(conversationResultString(outcome.RunOutput, "summary")) == "" {
		return errors.New("tool feedback requires a user-visible final explanation")
	}
	return nil
}

// Failure explanation/reporting must keep the tool's useful cause even if the
// final model output is unavailable or invalid. The cause is already sanitized
// at the ActionWorker boundary; only bounded feedback, never arguments, is used.
func recordedToolFailure(checkpoint map[string]interface{}) string {
	if final, ok := checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{}); ok {
		message, _ := final["message"].(string)
		return toolFeedbackMessage(message)
	}
	if feedback, ok := ReadToolFeedbackCorrection(checkpoint); ok {
		return feedback.Message
	}
	return ""
}
