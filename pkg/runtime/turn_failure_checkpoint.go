package runtime

// This file is the old per-Turn failure checkpoint path. Nothing in the run
// loop reads or writes it: loop Runs carry AgentRun.FailureExplanation
// (final_failure_explanation.go). It stays only while its old callers exist
// (hosted_turn*.go and turn_coordinator.go: loop-10; tool_feedback_correction.go,
// action_history.go, action_failure_evidence.go: del-1b;
// source_access_challenge.go: del-1a; conversation_run.go: CONV-07;
// catalog_turn_resolver.go, agent_run_worker.go, dependency.go,
// collaboration.go). Whoever deletes the last caller deletes this file.

import (
	"context"
	"errors"
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

// ValidateHostedTurnFinalFailureExplanation enforces the trusted failure
// boundary independently of model instructions and capability projection.
func ValidateHostedTurnFinalFailureExplanation(request HostedTurnRequest, response *HostedTurnResponse) error {
	if !requiresFinalFailureExplanation(request.ContinuationCheckpoint) {
		return nil
	}
	if response == nil || response.NextRunStatus != AgentRunStatusCompleted || response.WakeCondition != nil ||
		response.ProposedAction != nil || response.ProposedWorkspaceOperation != nil || response.ProposedFork != nil ||
		response.ProposedDelegation != nil || response.ProposedRunbook != nil || response.ProposedTask != nil || response.EvidenceGrounding != nil {
		return errors.New("a failed attempt requires one final explanation without actions or further work")
	}
	if silent, _ := response.RunOutput["silent"].(bool); silent {
		return errors.New("a final failure explanation must be visible to the user")
	}
	if _, present := response.RunOutput[AgentRequestDecisionOutputKey]; present {
		return errors.New("a final failure explanation cannot repeat an intake decision")
	}
	if strings.TrimSpace(response.OutputSummary) == "" && strings.TrimSpace(conversationResultString(response.RunOutput, "summary")) == "" {
		return errors.New("a failed attempt requires a user-visible final explanation")
	}
	return nil
}

func validateFinalFailureExplanationOutcome(run *AgentRun, outcome *TurnOutcome) error {
	if !requiresFinalFailureExplanation(run.Checkpoint) {
		return nil
	}
	if outcome == nil || !isTerminalAgentRunStatus(outcome.NextRunStatus) || outcome.WakeCondition != nil ||
		len(outcome.ProposedActions) != 0 || outcome.ProposedFork != nil || outcome.ProposedDelegation != nil || outcome.ProposedRunbook != nil || outcome.ProposedTask != nil {
		return errors.New("a failed attempt cannot continue or propose more work")
	}
	return nil
}

// FinalFailureExplanationSystemInstruction defines the generic hosted form contract.
// A host using a text-only explanation subcall can replace this exact instruction.
const FinalFailureExplanationSystemInstruction = "This attempt has stopped after the kernel-recorded failure in continuationCheckpoint._opensealFinalFailureExplanation. Give one brief, useful final reply in your configured voice, explaining what did not work and any next step the user actually needs. Ground the explanation in the recorded failure and succeeded evidence. Do not expose internal tool names, IDs, or governance details; do not guess that the user's billing plan, account, or permissions are wrong without evidence. Do not attempt, retry, repair, request setup, schedule, wait, fork, or delegate further work. Return nextRunStatus completed, no wakeCondition or proposals, and your final reply in runOutput.summary. Earlier instructions to continue work no longer apply to this failed attempt."

func projectFinalFailureExplanation(request *HostedTurnRequest) {
	request.Actions = nil
	request.EligibleAgents = nil
	request.Workspace = nil
	request.WorkspaceOperations = nil
	request.RunbookOperations = nil
	request.ConversationTasks = nil
	request.SkillPrompts = nil
	request.PendingInterventions = nil
	request.ModelMedia = nil
	request.SourceAccessChallenge = nil
	request.CanAskConversationQuestion = false
	request.SystemInstructions = append(request.SystemInstructions,
		FinalFailureExplanationSystemInstruction)
}

// runFinalFailureExplanation asks the model, with no actions or other work
// on offer, to explain the recorded failure to the user. The kernel's own
// reply is only the fallback when that answer cannot be had: the host or model
// is unavailable or refused, or its answer was not an answer-only
// explanation. A credit stop is returned as is, so the Run stops with its
// typed credit cause.
func (r *HostedTurnRunner) runFinalFailureExplanation(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	request, err := r.buildRequest(input)
	var response *HostedTurnResponse
	if err == nil {
		response, err = r.host.ExecuteHostedTurn(ctx, request)
	}
	if err != nil {
		var failure *TurnHostFailure
		if ctx.Err() != nil || errors.As(err, &failure) && creditStopFailureCode(failure.Code) {
			return nil, err
		}
		return finalFailureFallback(input.Run, nil, err), nil
	}
	if response == nil || response.APIVersion != HostedTurnAPIVersion || response.InvocationID != input.Turn.ID {
		return finalFailureFallback(input.Run, nil, errors.New("turn host returned a mismatched response envelope")), nil
	}
	if err := ValidateHostedTurnFinalFailureExplanation(request, response); err != nil {
		return finalFailureFallback(input.Run, response, err), nil
	}
	provider, model, err := normalizeModelIdentity(response.ModelProvider, response.Model)
	if err == nil && (provider == "" || model == "") {
		err = errors.New("turn host must report the actual model provider and model")
	}
	if err == nil {
		err = response.Usage.Validate()
	}
	if err != nil {
		return finalFailureFallback(input.Run, response, err), nil
	}
	answer := strings.TrimSpace(conversationResultString(response.RunOutput, "summary"))
	if answer == "" {
		answer = strings.TrimSpace(response.OutputSummary)
	}
	return &TurnOutcome{
		ModelProvider: provider, Model: model, SkillSelections: response.SkillSelections, Decisions: response.Decisions,
		NextRunStatus: AgentRunStatusCompleted, OutputSummary: answer,
		RunOutput:              map[string]interface{}{"summary": answer},
		ContinuationCheckpoint: preserveKernelActionHistory(request.ContinuationCheckpoint, response.ContinuationCheckpoint),
		Usage:                  response.Usage,
	}, nil
}

// finalFailureFallback is the kernel's reply when the model could not explain
// the failure. Why the model answer was not used is recorded on the Turn.
func finalFailureFallback(run *AgentRun, response *HostedTurnResponse, cause error) *TurnOutcome {
	outcome, _ := terminalFailureOutcome(run)
	reason := "The model could not write the failure explanation"
	if cause != nil {
		reason += ": " + toolFeedbackMessage(cause.Error())
	}
	outcome.Decisions = append(outcome.Decisions, TurnDecision{Summary: boundedTurnFailureText(reason)})
	if response != nil && response.Usage.Validate() == nil {
		outcome.Usage = response.Usage
	}
	return outcome
}

func checkpointTerminalFailure(checkpoint map[string]interface{}, code string) map[string]interface{} {
	result := checkpointFinalFailureExplanation(checkpoint, "execution", "The requested work did not complete.")
	failure := result[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	failure["code"] = terminalFailureCode(code)
	return result
}

// terminalFailureOutcome is the kernel-authored reply of a stopped attempt.
// A failure returned to the model for correction is not stopped; a typed
// source challenge continues only through its verified interaction, which
// callers check before this.
func terminalFailureOutcome(run *AgentRun) (*TurnOutcome, bool) {
	if run == nil || (!requiresFinalFailureExplanation(run.Checkpoint) && !sourceAccessChallengeFeedback(run.Checkpoint)) {
		return nil, false
	}
	code := terminalFailureCodeFromCheckpoint(run.Checkpoint)
	checkpoint := checkpointTerminalFailure(run.Checkpoint, code)
	return &TurnOutcome{
		NextRunStatus: AgentRunStatusCompleted, OutputSummary: terminalFailureReplyFromCheckpoint(checkpoint),
		RunOutput:              map[string]interface{}{"summary": terminalFailureReplyFromCheckpoint(checkpoint)},
		ContinuationCheckpoint: checkpoint, ModelProvider: "host", Model: "terminal-failure-report",
	}, true
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
