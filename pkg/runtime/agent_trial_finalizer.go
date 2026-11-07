package runtime

import (
	"context"
	"errors"
	"time"
)

// AgentTrialFinalizer hands a terminal try_agent child's reply to its parent
// builder Run: it appends the AgentTrialReceipt to the parent and queues the
// parent when it waits on that trial. It wraps the host's terminal finalizer,
// which runs for every Run as before.
type AgentTrialFinalizer struct {
	store RunCommandStore
	next  RunTerminalFinalizer
	// wake nudges the scope's workers after a parent is queued (optional).
	wake func(Scope)
	now  func() time.Time
}

// NewAgentTrialFinalizer wraps next (may be nil). wake may be nil.
func NewAgentTrialFinalizer(store RunCommandStore, next RunTerminalFinalizer, wake func(Scope)) *AgentTrialFinalizer {
	return &AgentTrialFinalizer{store: store, next: next, wake: wake, now: time.Now}
}

func (f *AgentTrialFinalizer) FinalizeRun(ctx context.Context, run *AgentRun) error {
	trialErr := f.settleAgentTrial(ctx, run)
	var nextErr error
	if f != nil && f.next != nil {
		nextErr = f.next.FinalizeRun(ctx, run)
	}
	return errors.Join(trialErr, nextErr)
}

// settleAgentTrial delivers the receipt of a terminal trial child. It is
// idempotent: the receipt id is the child's, and a parent that already holds
// it is left alone. A parent that is not waiting on the trial (its step has
// not parked yet) keeps its status; the appended receipt bumps its revision
// so its park CAS fails and re-plans. A canceled child is delivered only to
// a parent waiting on it: a parent that canceled it at the deadline already
// reported timed_out.
func (f *AgentTrialFinalizer) settleAgentTrial(ctx context.Context, child *AgentRun) error {
	if f == nil || f.store == nil || child == nil || !isTerminalAgentRunStatus(child.Status) {
		return nil
	}
	trial, ok, err := agentTrialContextForRun(child)
	if err != nil || !ok {
		return err
	}
	if child.ParentRunID != trial.ParentRunID {
		return nil
	}
	interventionID := agentTrialInterventionPrefix + child.ID
	for attempt := 0; attempt < 4; attempt++ {
		parent, err := getAgentTrialChild(ctx, f.store, child.Scope, trial.ParentRunID)
		if err != nil {
			return err
		}
		if parent == nil || isTerminalAgentRunStatus(parent.Status) || interactionReceiptDelivered(parent, interventionID) {
			return nil
		}
		waiting := parent.Status == AgentRunStatusWaitingForEvent && parent.WakeCondition != nil &&
			parent.WakeCondition.Type == AgentTrialWakeType && parent.WakeCondition.Reference == child.ID
		if child.Status == AgentRunStatusCanceled && !waiting {
			return nil
		}
		now := f.now().UTC()
		receipt := agentTrialReceiptFromRun(child, now)
		status, wake := AgentRunStatusQueued, (*WakeCondition)(nil)
		if !waiting {
			status, wake = parent.Status, parent.WakeCondition
		}
		actor := ActivityActor{Type: "service", ID: "agent-trial-finalizer"}
		_, _, err = NewRunActivityService(f.store, f.store).TransitionRun(ctx, parent.Scope, parent.ID, RunTransitionRequest{
			ExpectedRevision: parent.Revision, Status: status, WakeCondition: wake,
			Summary: "Received the trial reply; continuing", EventType: "run.agent_trial_settled",
			Actor: actor, CorrelationID: child.ID, CausationID: trial.ActionCallID,
			Payload: map[string]interface{}{"trialRunId": child.ID, "status": receipt.Status},
			Intervention: &AgentRunIntervention{
				ID: interventionID, Actor: actor, CreatedAt: now, ActionCallID: trial.ActionCallID, AgentTrial: &receipt,
			},
		})
		if errors.Is(err, ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return err
		}
		if waiting && f.wake != nil {
			f.wake(parent.Scope)
		}
		return nil
	}
	return ErrRevisionConflict
}

func interactionReceiptDelivered(run *AgentRun, interventionID string) bool {
	for _, intervention := range run.PendingInterventions {
		if intervention.ID == interventionID {
			return true
		}
	}
	return false
}
