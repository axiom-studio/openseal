package runtime

import (
	"context"
	"errors"
)

// Unprovable legacy executable identity requires explicit operator recovery.
// It is neither a terminal work result nor an automatically retried failure.
// Keep the accepted inputs and committed continuation untouched for review.
func (p *AgentRunWorkerPool) parkAcceptedRunRecovery(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, cause error) bool {
	if run == nil || (!errors.Is(cause, ErrAcceptedRunExecution) && !errors.Is(cause, ErrRunSkillDependencyUnavailable)) {
		return false
	}
	turnID := ""
	if turn != nil {
		turnID = turn.ID
	}
	_, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusPaused, LeaseOwner: workerID,
		Error:   "The accepted executable identity requires review before this Run can continue.",
		Summary: "Run paused for review of its accepted executable identity", EventType: "run.execution_recovery_required",
		Actor: ActivityActor{Type: "worker", ID: workerID}, TurnID: turnID, CausationID: turnID,
		Payload: map[string]interface{}{"classification": "accepted_execution_unavailable"},
	})
	if err != nil {
		p.logger.Warnw("failed to pause Run requiring executable identity recovery", "runId", run.ID, "error", err)
	}
	return true
}
