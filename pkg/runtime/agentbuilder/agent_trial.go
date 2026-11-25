package agentbuilder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
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
	AgentTrialWaitSummary        = "Trying the draft"
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
func (d *ActionDispatcher) tryAgent(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun) (map[string]interface{}, error) {
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
func ensureAgentTrialRun(ctx context.Context, store models.PortfolioStore, parent *models.AgentRun, callID string, args models.AgentTryArguments, now time.Time) (*models.AgentRun, error) {
	childID := agentTrialRunID(parent.ID, callID)
	existing, err := AgentTrialChild(ctx, store, parent.Scope, childID)
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
	child, err := portfolio.BuildAgentRun(ctx, store, models.CreateAgentRunRequest{
		Kind: models.RunKindAgentWork, Scope: parent.Scope, ParentRunID: parent.ID,
		Owner: parent.Owner, AssignedAgentID: parent.AssignedAgentID, ActingUser: parent.ActingUser,
		Goal: args.Message, Source: models.RunSourceHandoff, Priority: parent.Priority,
		Context: map[string]interface{}{AgentTrialContextKey: trial, AgentTrialModelContextKey: args.Model},
		Budget:  &models.BudgetPolicy{MaxTurns: agentTrialMaxTurns},
	}, childID, now)
	if err != nil {
		return nil, err
	}
	if err := store.CreateAgentRun(ctx, child); err != nil {
		// A concurrent attempt of the same call may have created it.
		if existing, getErr := AgentTrialChild(ctx, store, parent.Scope, childID); getErr == nil && existing != nil {
			return checkedAgentTrialChild(existing, parent.ID, callID)
		}
		return nil, err
	}
	return child, nil
}

// AgentTrialChild reads the trial child Run childID; it is nil when the
// Run does not exist.
func AgentTrialChild(ctx context.Context, store models.PortfolioStore, scope models.Scope, childID string) (*models.AgentRun, error) {
	child, err := store.GetAgentRun(ctx, scope, childID)
	if errors.Is(err, models.ErrRunNotFound) {
		return nil, nil
	}
	return child, err
}

func checkedAgentTrialChild(child *models.AgentRun, parentRunID, callID string) (*models.AgentRun, error) {
	trial, ok, err := AgentTrialContextForRun(child)
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

// AgentTrialContextForRun decodes a trial child's AgentTrialContext; ok is
// false for every other Run.
func AgentTrialContextForRun(run *models.AgentRun) (*AgentTrialContext, bool, error) {
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

// AgentTrialSessionSeed seeds a trial child's run session with the test
// message as it is, the way a person would send it.
func AgentTrialSessionSeed(run *models.AgentRun) (*models.SessionEntry, error) {
	message := strings.TrimSpace(run.Goal)
	if message == "" {
		return nil, fmt.Errorf("%w: agent trial needs a message", models.ErrInvalidAgentRun)
	}
	key := models.GoalSourceKey(run.ID)
	return &models.SessionEntry{
		ID: models.SessionEntryID(models.RunAgentSessionID(run.Scope, run.ID), key), RunID: run.ID,
		Type: models.SessionEntryUser, SourceKey: key,
		User: &models.UserEntry{
			Origin:  models.UserEntryGoal,
			Author:  models.SessionAuthor{Type: "service", ID: "agent-trial"},
			Content: []models.ContentPart{{Type: models.ContentPartText, Text: models.TruncateUTF8(message, models.MaximumChildBriefBytes)}},
		},
	}, nil
}

// IsAgentTrialCall reports a succeeded try_agent call.
func IsAgentTrialCall(call *models.ActionCall) bool {
	return call != nil && call.Status == models.ActionCallStatusSucceeded && call.DuplicateOfActionCallID == "" &&
		call.SkillID == models.AgentManagementSkillID && call.Action == models.AgentActionTryAgent
}

// AgentTrialCallRunID is the trial child Run a try_agent call started.
func AgentTrialCallRunID(call *models.ActionCall) string {
	id, _ := call.Output["trialRunId"].(string)
	return strings.TrimSpace(id)
}

// AgentTrialDeadline is when the parent stops waiting for child.
func AgentTrialDeadline(child *models.AgentRun) time.Time {
	return child.CreatedAt.UTC().Add(agentTrialTimeout)
}

// AgentTrialTimedOut is the receipt of a trial child that missed its
// deadline.
func AgentTrialTimedOut(childID string) models.AgentTrialReceipt {
	return models.AgentTrialReceipt{RunID: childID, Status: AgentTrialStatusTimedOut, FailureMessage: "The draft did not answer within 5 minutes."}
}

// AgentTrialReceiptFor finds the latest receipt for callID's trial child in
// interventions; it is nil when there is none.
func AgentTrialReceiptFor(callID, childID string, interventions []models.AgentRunIntervention) *models.AgentTrialReceipt {
	for i := len(interventions) - 1; i >= 0; i-- {
		intervention := interventions[i]
		if intervention.ActionCallID == callID && intervention.AgentTrial != nil && intervention.AgentTrial.RunID == childID {
			receipt := *intervention.AgentTrial
			return &receipt
		}
	}
	return nil
}

// AgentTrialReceiptFromRun is the receipt of a terminal trial child. A
// canceled child that ran to its deadline timed out; one canceled sooner was
// stopped.
func AgentTrialReceiptFromRun(child *models.AgentRun, now time.Time) models.AgentTrialReceipt {
	receipt := models.AgentTrialReceipt{RunID: child.ID}
	switch child.Status {
	case models.AgentRunStatusCompleted:
		receipt.Status = AgentTrialStatusCompleted
		receipt.Answer = ChildRunAnswer(child)
	case models.AgentRunStatusCanceled:
		ended := now
		if child.CompletedAt != nil {
			ended = *child.CompletedAt
		} else if !child.UpdatedAt.IsZero() {
			ended = child.UpdatedAt
		}
		if !ended.Before(AgentTrialDeadline(child)) {
			return AgentTrialTimedOut(child.ID)
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

// AgentTrialToolOutput is the model-facing try_agent result.
func AgentTrialToolOutput(receipt models.AgentTrialReceipt) map[string]interface{} {
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
	return models.TruncateUTF8(text, agentTrialReplyBytes-len(suffix)) + suffix
}

// ChildRunAnswer is a child's final answer: its summary.
func ChildRunAnswer(child *models.AgentRun) string {
	text, _ := child.Output[conversationrun.FinalOutputSummaryKey].(string)
	return strings.TrimSpace(text)
}

// TrialCalls is how the run loop waits on try_agent calls: the parent parks
// on the agent_trial card while the child runs, and the tool result is the
// child's reply. It is the run loop's ChildRunCall for try_agent.
type TrialCalls struct {
	runs models.RunCommandStore
	now  func() time.Time
}

// NewTrialCalls reads (and at the deadline cancels) trial children in runs;
// now nil is time.Now.
func NewTrialCalls(runs models.RunCommandStore, now func() time.Time) (*TrialCalls, error) {
	if runs == nil {
		return nil, errors.New("try_agent trial calls need a run store")
	}
	if now == nil {
		now = time.Now
	}
	return &TrialCalls{runs: runs, now: now}, nil
}

// Claims reports a succeeded try_agent call.
func (t *TrialCalls) Claims(call *models.ActionCall) bool { return IsAgentTrialCall(call) }

// Wait is the agent_trial card of a succeeded try_agent call while its
// child runs and its deadline has not passed. Past the deadline it returns
// nil: the resume cancels the child and reports timed_out.
func (t *TrialCalls) Wait(ctx context.Context, call *models.ActionCall) (*models.WakeCondition, string, error) {
	childID := AgentTrialCallRunID(call)
	if childID == "" {
		return nil, "", nil
	}
	child, err := AgentTrialChild(ctx, t.runs, call.Scope, childID)
	if err != nil || child == nil || models.IsTerminalAgentRunStatus(child.Status) {
		return nil, "", err
	}
	deadline := AgentTrialDeadline(child)
	if !t.now().Before(deadline) {
		return nil, "", nil
	}
	return &models.WakeCondition{Type: agentrun.AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}, AgentTrialWaitSummary, nil
}

// Output is the tool output of a settled try_agent call: the receipt the
// finalizer delivered, else the child read directly (the finalizer missed
// it). A child still running past its deadline is canceled and reported
// timed_out.
func (t *TrialCalls) Output(ctx context.Context, run *models.AgentRun, call *models.ActionCall, settled []models.AgentRunIntervention) (map[string]interface{}, error) {
	childID := AgentTrialCallRunID(call)
	if childID == "" {
		return nil, fmt.Errorf("try_agent call %s has no trial Run", call.ID)
	}
	if receipt := AgentTrialReceiptFor(call.ID, childID, settled); receipt != nil {
		return AgentTrialToolOutput(*receipt), nil
	}
	if run != nil {
		if receipt := AgentTrialReceiptFor(call.ID, childID, run.PendingInterventions); receipt != nil {
			return AgentTrialToolOutput(*receipt), nil
		}
	}
	child, err := AgentTrialChild(ctx, t.runs, call.Scope, childID)
	if err != nil {
		return nil, err
	}
	if child == nil {
		return AgentTrialToolOutput(models.AgentTrialReceipt{RunID: childID, Status: AgentTrialStatusFailed, FailureMessage: "The trial Run is missing."}), nil
	}
	if !models.IsTerminalAgentRunStatus(child.Status) {
		ended, canceled, err := t.cancel(ctx, child)
		if err != nil {
			return nil, err
		}
		if canceled {
			return AgentTrialToolOutput(AgentTrialTimedOut(child.ID)), nil
		}
		child = ended
	}
	return AgentTrialToolOutput(AgentTrialReceiptFromRun(child, t.now())), nil
}

// cancel cancels a trial child that outlived its deadline; canceled reports
// that this call stopped it. A child that ended meanwhile is returned as it
// ended.
func (t *TrialCalls) cancel(ctx context.Context, child *models.AgentRun) (*models.AgentRun, bool, error) {
	result, err := agentrun.NewRunCommandService(t.runs, models.WithClock(t.now)).CommandAgentRun(ctx, agentrun.CommandRequest{
		Scope: child.Scope, RunID: child.ID, ExpectedRevision: child.Revision, Kind: agentrun.CommandCancel,
		Actor:   models.ActivityActor{Type: "runtime", ID: "agent-trial"},
		Summary: "Canceled: the trial did not answer in time", Visibility: models.ActivityVisibilityScope,
	})
	if err == nil && result != nil && result.Run != nil && result.Run.Status == models.AgentRunStatusCanceled {
		return result.Run, true, nil
	}
	reloaded, getErr := AgentTrialChild(ctx, t.runs, child.Scope, child.ID)
	if getErr != nil {
		return nil, false, errors.Join(err, getErr)
	}
	if reloaded != nil && models.IsTerminalAgentRunStatus(reloaded.Status) {
		return reloaded, false, nil
	}
	if err == nil {
		err = fmt.Errorf("trial Run %s did not stop", child.ID)
	}
	return nil, false, err
}
