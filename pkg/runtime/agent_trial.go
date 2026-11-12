package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

// try_agent runs a drafted profile as a bounded child Run of the builder:
// one test message, no tools, no definition registered (spec-openseal §3.3).
// The parent waits on the agent_trial card until the child ends, and the
// tool result is the child's reply.

const (
	// agentTrialTimeout is how long the builder waits for a trial before
	// it cancels the child and reports timed_out.
	agentTrialTimeout = 5 * time.Minute
	// agentTrialMaxTurns bounds the child: one reply, plus one step to
	// recover from a malformed final answer.
	agentTrialMaxTurns           = 2
	agentTrialWaitSummary        = "Trying the draft"
	agentTrialReplyBytes         = 8 << 10
	agentTrialInterventionPrefix = "agent-trial-result:"
)

// Trial outcomes, as the receipt and the tool result carry them.
const (
	AgentTrialStatusCompleted = "completed"
	AgentTrialStatusFailed    = "failed"
	AgentTrialStatusTimedOut  = "timed_out"
)

// agentTrialRunID is the child Run id of a try_agent call; a replay of the
// call finds the same child.
func agentTrialRunID(parentRunID, callID string) string {
	return stableForkIdentifier(parentRunID, callID, "trial")
}

// tryAgent starts the trial child Run and returns at once; the parent parks
// on the agent_trial card.
func (d *AgentBuilderActionDispatcher) tryAgent(ctx context.Context, input ActionDispatchInput, run *AgentRun) (map[string]interface{}, error) {
	call := input.Call
	args, err := resolveAgentTry(ctx, d.host, call.Scope, input.Arguments)
	if err != nil {
		return nil, err
	}
	child, err := ensureAgentTrialRun(ctx, d.store, run, call.ID, args, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return agentBuilderJSONMap(map[string]interface{}{
		"resourceType": "agent_trial", "trialRunId": child.ID, "status": "running",
	})
}

// ensureAgentTrialRun creates the trial child of parent for callID, or
// returns the one an earlier attempt created.
func ensureAgentTrialRun(ctx context.Context, store PortfolioStore, parent *AgentRun, callID string, args agentTryArguments, now time.Time) (*AgentRun, error) {
	childID := agentTrialRunID(parent.ID, callID)
	existing, err := getAgentTrialChild(ctx, store, parent.Scope, childID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return checkedAgentTrialChild(existing, parent.ID, callID)
	}
	profile := args.AgentProfileArguments
	profile.Model = ""
	trial, err := agentTrialContextMap(AgentTrialContext{Profile: profile, ParentRunID: parent.ID, ActionCallID: callID})
	if err != nil {
		return nil, err
	}
	child, err := buildAgentRun(ctx, store, CreateAgentRunRequest{
		Kind: RunKindAgentWork, Scope: parent.Scope, ParentRunID: parent.ID,
		Owner: parent.Owner, AssignedAgentID: parent.AssignedAgentID, ActingUser: parent.ActingUser,
		Goal: args.Message, Source: RunSourceHandoff, Priority: parent.Priority,
		Context: map[string]interface{}{AgentTrialContextKey: trial, AgentTrialModelContextKey: args.Model},
		Budget:  &BudgetPolicy{MaxTurns: agentTrialMaxTurns},
	}, childID, now)
	if err != nil {
		return nil, err
	}
	if err := store.CreateAgentRun(ctx, child); err != nil {
		// A concurrent attempt of the same call may have created it.
		if existing, getErr := getAgentTrialChild(ctx, store, parent.Scope, childID); getErr == nil && existing != nil {
			return checkedAgentTrialChild(existing, parent.ID, callID)
		}
		return nil, err
	}
	return child, nil
}

func getAgentTrialChild(ctx context.Context, store PortfolioStore, scope Scope, childID string) (*AgentRun, error) {
	child, err := store.GetAgentRun(ctx, scope, childID)
	if errors.Is(err, ErrRunNotFound) {
		return nil, nil
	}
	return child, err
}

func checkedAgentTrialChild(child *AgentRun, parentRunID, callID string) (*AgentRun, error) {
	trial, ok, err := agentTrialContextForRun(child)
	if err != nil {
		return nil, err
	}
	if !ok || child.ParentRunID != parentRunID || trial.ParentRunID != parentRunID || trial.ActionCallID != callID {
		return nil, fmt.Errorf("run %s already exists and is not this call's trial", child.ID)
	}
	return child, nil
}

func agentTrialContextMap(trial AgentTrialContext) (map[string]interface{}, error) {
	encoded, err := json.Marshal(trial)
	if err != nil {
		return nil, err
	}
	var value map[string]interface{}
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// agentTrialContextForRun decodes a trial child's AgentTrialContext; ok is
// false for every other Run.
func agentTrialContextForRun(run *AgentRun) (*AgentTrialContext, bool, error) {
	if run == nil || run.Context == nil {
		return nil, false, nil
	}
	raw, present := run.Context[AgentTrialContextKey]
	if !present {
		return nil, false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, true, fmt.Errorf("decode agent trial context: %w", err)
	}
	var trial AgentTrialContext
	if err := json.Unmarshal(encoded, &trial); err != nil {
		return nil, true, fmt.Errorf("decode agent trial context: %w", err)
	}
	if strings.TrimSpace(trial.ParentRunID) == "" || strings.TrimSpace(trial.ActionCallID) == "" ||
		strings.TrimSpace(trial.Profile.SystemPrompt) == "" {
		return nil, true, errors.New("agent trial context is incomplete")
	}
	return &trial, true, nil
}

// agentTrial binds a trial child: the builder's deployment and definition
// identity (the host's model grant checks it), the drafted persona as the
// Agent instructions, the drafted model, and no actions, workspace tools or
// control tools beyond the final answer.
func (r *loopBindingResolution) agentTrial(ctx context.Context, trial *AgentTrialContext) (*TurnRunnerBinding, error) {
	if r.run.Kind != RunKindAgentWork || r.run.ParentRunID == "" || r.run.ParentRunID != trial.ParentRunID {
		return nil, errors.New("agent trial context does not belong to this Run")
	}
	profile := trial.Profile
	r.definition = &kernelagent.AgentDefinition{
		ID: r.definition.ID, Version: r.definition.Version,
		DisplayName: profile.DisplayName, Purpose: profile.Purpose, SystemPrompt: profile.SystemPrompt,
		Personality: profile.Personality, OperatingPrinciples: append([]string(nil), profile.OperatingPrinciples...),
	}
	r.trial = true
	binding := r.identity(r.deployment.ID)
	binding.Model, _ = r.run.Context[AgentTrialModelContextKey].(string)
	binding.Model = strings.TrimSpace(binding.Model)
	return r.model(ctx, binding, nil, PromptContribution{}, false)
}

// agentTrialSessionSeed seeds a trial child's run session with the test
// message as it is, the way a person would send it.
func agentTrialSessionSeed(run *AgentRun) (*SessionEntry, error) {
	message := strings.TrimSpace(run.Goal)
	if message == "" {
		return nil, fmt.Errorf("%w: agent trial needs a message", ErrInvalidAgentRun)
	}
	key := GoalSourceKey(run.ID)
	return &SessionEntry{
		ID: SessionEntryID(RunAgentSessionID(run.Scope, run.ID), key), RunID: run.ID,
		Type: SessionEntryUser, SourceKey: key,
		User: &UserEntry{
			Origin:  UserEntryGoal,
			Author:  SessionAuthor{Type: "service", ID: "agent-trial"},
			Content: []ContentPart{{Type: ContentPartText, Text: cutUTF8(message, maximumChildBriefBytes)}},
		},
	}, nil
}

// isAgentTrialCall reports a succeeded try_agent call.
func isAgentTrialCall(call *ActionCall) bool {
	return call != nil && call.Status == ActionCallStatusSucceeded && call.DuplicateOfActionCallID == "" &&
		call.SkillID == AgentManagementSkillID && call.Action == AgentActionTryAgent
}

func agentTrialCallRunID(call *ActionCall) string {
	id, _ := call.Output["trialRunId"].(string)
	return strings.TrimSpace(id)
}

func agentTrialDeadline(child *AgentRun) time.Time {
	return child.CreatedAt.UTC().Add(agentTrialTimeout)
}

// agentTrialWait is the agent_trial card of a succeeded try_agent call while
// its child runs and its deadline has not passed. Past the deadline it
// returns nil: the resume cancels the child and reports timed_out.
func (p *governedParker) agentTrialWait(ctx context.Context, call *ActionCall) (*WakeCondition, string, error) {
	if p.runs == nil {
		return nil, "", nil
	}
	childID := agentTrialCallRunID(call)
	if childID == "" {
		return nil, "", nil
	}
	child, err := getAgentTrialChild(ctx, p.runs, call.Scope, childID)
	if err != nil || child == nil || isTerminalAgentRunStatus(child.Status) {
		return nil, "", err
	}
	deadline := agentTrialDeadline(child)
	if !p.now().Before(deadline) {
		return nil, "", nil
	}
	return &WakeCondition{Type: AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}, agentTrialWaitSummary, nil
}

// agentTrialOutput is the tool output of a settled try_agent call: the
// receipt the finalizer delivered, else the child read directly (the
// finalizer missed it). A child still running past its deadline is canceled
// and reported timed_out.
func (p *governedParker) agentTrialOutput(ctx context.Context, run *AgentRun, call *ActionCall, settled []AgentRunIntervention) (map[string]interface{}, error) {
	childID := agentTrialCallRunID(call)
	if childID == "" {
		return nil, fmt.Errorf("try_agent call %s has no trial Run", call.ID)
	}
	if receipt := agentTrialReceiptFor(call.ID, childID, settled); receipt != nil {
		return agentTrialToolOutput(*receipt), nil
	}
	if run != nil {
		if receipt := agentTrialReceiptFor(call.ID, childID, run.PendingInterventions); receipt != nil {
			return agentTrialToolOutput(*receipt), nil
		}
	}
	if p.runs == nil {
		return nil, errors.New("governed parker cannot read the trial Run")
	}
	child, err := getAgentTrialChild(ctx, p.runs, call.Scope, childID)
	if err != nil {
		return nil, err
	}
	if child == nil {
		return agentTrialToolOutput(AgentTrialReceipt{RunID: childID, Status: AgentTrialStatusFailed, FailureMessage: "The trial Run is missing."}), nil
	}
	if !isTerminalAgentRunStatus(child.Status) {
		ended, canceled, err := p.cancelAgentTrial(ctx, child)
		if err != nil {
			return nil, err
		}
		if canceled {
			return agentTrialToolOutput(agentTrialTimedOut(child.ID)), nil
		}
		child = ended
	}
	return agentTrialToolOutput(agentTrialReceiptFromRun(child, p.now())), nil
}

// cancelAgentTrial cancels a trial child that outlived its deadline;
// canceled reports that this call stopped it. A child that ended meanwhile
// is returned as it ended.
func (p *governedParker) cancelAgentTrial(ctx context.Context, child *AgentRun) (*AgentRun, bool, error) {
	result, err := NewRunCommandService(p.runs).CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: child.Scope, RunID: child.ID, ExpectedRevision: child.Revision, Kind: AgentRunCommandCancel,
		Actor:   ActivityActor{Type: "runtime", ID: "agent-trial"},
		Summary: "Canceled: the trial did not answer in time", Visibility: ActivityVisibilityScope,
	})
	if err == nil && result != nil && result.Run != nil && result.Run.Status == AgentRunStatusCanceled {
		return result.Run, true, nil
	}
	reloaded, getErr := getAgentTrialChild(ctx, p.runs, child.Scope, child.ID)
	if getErr != nil {
		return nil, false, errors.Join(err, getErr)
	}
	if reloaded != nil && isTerminalAgentRunStatus(reloaded.Status) {
		return reloaded, false, nil
	}
	if err == nil {
		err = fmt.Errorf("trial Run %s did not stop", child.ID)
	}
	return nil, false, err
}

func agentTrialTimedOut(childID string) AgentTrialReceipt {
	return AgentTrialReceipt{RunID: childID, Status: AgentTrialStatusTimedOut, FailureMessage: "The draft did not answer within 5 minutes."}
}

func agentTrialReceiptFor(callID, childID string, interventions []AgentRunIntervention) *AgentTrialReceipt {
	for i := len(interventions) - 1; i >= 0; i-- {
		intervention := interventions[i]
		if intervention.ActionCallID == callID && intervention.AgentTrial != nil && intervention.AgentTrial.RunID == childID {
			receipt := *intervention.AgentTrial
			return &receipt
		}
	}
	return nil
}

// agentTrialReceiptFromRun is the receipt of a terminal trial child. A
// canceled child that ran to its deadline timed out; one canceled sooner was
// stopped.
func agentTrialReceiptFromRun(child *AgentRun, now time.Time) AgentTrialReceipt {
	receipt := AgentTrialReceipt{RunID: child.ID}
	switch child.Status {
	case AgentRunStatusCompleted:
		receipt.Status = AgentTrialStatusCompleted
		receipt.Answer = forkChildAnswer(child)
	case AgentRunStatusCanceled:
		ended := now
		if child.CompletedAt != nil {
			ended = *child.CompletedAt
		} else if !child.UpdatedAt.IsZero() {
			ended = child.UpdatedAt
		}
		if !ended.Before(agentTrialDeadline(child)) {
			return agentTrialTimedOut(child.ID)
		}
		receipt.Status = AgentTrialStatusFailed
		receipt.FailureMessage = "The trial was stopped."
	default:
		receipt.Status = AgentTrialStatusFailed
		if child.FailureExplanation != nil {
			receipt.FailureMessage = strings.TrimSpace(child.FailureExplanation.Message)
		}
		if receipt.FailureMessage == "" {
			receipt.FailureMessage = strings.TrimSpace(child.Error)
		}
		if receipt.FailureMessage == "" {
			receipt.FailureMessage = "The trial failed."
		}
	}
	return receipt
}

// agentTrialToolOutput is the model-facing try_agent result.
func agentTrialToolOutput(receipt AgentTrialReceipt) map[string]interface{} {
	output := map[string]interface{}{
		"status": receipt.Status, "reply": truncateAgentTrialText(receipt.Answer, receipt.RunID), "trialRunId": receipt.RunID,
	}
	if receipt.FailureMessage != "" {
		output["error"] = truncateAgentTrialText(receipt.FailureMessage, receipt.RunID)
	}
	return output
}

func truncateAgentTrialText(text, runID string) string {
	if len(text) <= agentTrialReplyBytes {
		return text
	}
	suffix := "…[truncated; full reply is in run " + runID + "]"
	return cutUTF8(text, agentTrialReplyBytes-len(suffix)) + suffix
}
