package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
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
	return agentrun.StableForkIdentifier(parentRunID, callID, "trial")
}

// tryAgent starts the trial child Run and returns at once; the parent parks
// on the agent_trial card.
func (d *AgentBuilderActionDispatcher) tryAgent(ctx context.Context, input agentrun.ActionDispatchInput, run *AgentRun) (map[string]interface{}, error) {
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

const finalOutputReportKey = "report"

// forkChildAnswer is a child's final answer: its report, else its summary.
func forkChildAnswer(child *AgentRun) string {
	for _, key := range []string{finalOutputReportKey, conversationrun.FinalOutputSummaryKey} {
		if text, ok := child.Output[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
