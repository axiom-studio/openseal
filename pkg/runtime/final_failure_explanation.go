package runtime

import (
	"errors"
	"strings"
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
	request.WorkspaceCredentials = nil
	request.RunbookOperations = nil
	request.ConversationTasks = nil
	request.SkillPrompts = nil
	request.PendingInterventions = nil
	request.ModelMedia = nil
	request.SystemInstructions = append(request.SystemInstructions,
		FinalFailureExplanationSystemInstruction)
}

// An adapter's explicit top-level failure envelope is operational evidence,
// unlike the status of a fetched resource or a nested document. Some process
// Skills use success:false plus error instead of a transport error.
func explicitActionResultFailure(output map[string]interface{}) error {
	if succeeded, ok := output["success"].(bool); ok && !succeeded {
		if message, ok := output["error"].(string); ok && strings.TrimSpace(message) != "" {
			return errors.New(strings.TrimSpace(message))
		}
		return errors.New("The tool reported that its operation failed.")
	}
	if len(output) == 1 {
		if message, ok := output["error"].(string); ok && strings.TrimSpace(message) != "" {
			return errors.New(strings.TrimSpace(message))
		}
	}
	if failed, ok := output["isError"].(bool); ok && failed {
		return errors.New("The tool reported that its operation failed.")
	}
	return nil
}
