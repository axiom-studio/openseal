package runtime

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
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

// noModelTurnFailureCode names the host failures under which no model can
// answer this turn: the Workspace has no credits, or the model provider or
// its configuration is unavailable. Every other failure can be explained.
func noModelTurnFailureCode(code string) bool {
	switch terminalFailureCode(code) {
	case "workspace_credits_exhausted", "workspace_daily_limit_reached", "model_configuration_failed", "budget_exhausted",
		"provider_authentication_failed", "provider_access_denied", "provider_quota_exhausted", "provider_rate_limited", "provider_unavailable":
		return true
	}
	return false
}

const maximumTurnFailureTextBytes = 1024

func boundedTurnFailureText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maximumTurnFailureTextBytes {
		cut := maximumTurnFailureTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// modelExplainsFailure reports whether a stopped attempt of this Run gets an
// answer-only model turn to explain its failure before it ends. Hosted Agent
// work and conversations do; other kinds end with the kernel's reply.
func modelExplainsFailure(run *AgentRun) bool {
	return run != nil && (run.Kind == RunKindAgentWork || run.Kind == RunKindConversation)
}

// turnFailureCanBeExplained reports whether a failed turn leaves a model able
// to answer: not when the host or model provider is unavailable, the
// Workspace has no credits, the budget is spent, or the work was canceled.
func turnFailureCanBeExplained(err error, code string) bool {
	if err == nil || errors.Is(err, ErrTurnHostUnavailable) || errors.Is(err, ErrTurnHostConfiguration) ||
		errors.Is(err, ErrBudgetExhausted) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrTurnLeaseHeld) {
		return false
	}
	switch terminalFailureCode(code) {
	case "model_image_input_unsupported", "agent_behavior_fixed":
		// These have their own exact kernel reply.
		return false
	}
	return !noModelTurnFailureCode(code)
}

// creditStopFailureCode is a Workspace credit stop, whose typed cause the
// Run keeps even after an earlier tool failure.
func creditStopFailureCode(code string) bool {
	switch terminalFailureCode(code) {
	case "workspace_credits_exhausted", "workspace_daily_limit_reached":
		return true
	}
	return false
}
