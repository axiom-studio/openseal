package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

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
	return &WakeCondition{Type: agentrun.AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}, agentTrialWaitSummary, nil
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
	result, err := agentrun.NewRunCommandService(p.runs, models.WithClock(p.now)).CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
		Scope: child.Scope, RunID: child.ID, ExpectedRevision: child.Revision, Kind: agentrun.AgentRunCommandCancel,
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
