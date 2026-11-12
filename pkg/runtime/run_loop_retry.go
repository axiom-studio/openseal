package runtime

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// Host retry (D13): a step whose host was unavailable is failed with
// FailureCode host_unavailable and the Run is requeued after a short backoff.
// The count of consecutive retries derives only from the durable step rows,
// so a worker restart schedules the same attempt; a completed step resets it.

// hostRetryDelays are the waits before each requeued attempt. Once they are
// used up, the next unavailable host fails the Run.
var hostRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

const hostUnavailableMessage = "The agent's step host is unavailable, so the run stopped after retrying."

// consecutiveHostUnavailableSteps counts the newest step rows of the Run at
// or before sequence through, newest first, while each failed with
// host_unavailable. It reads at most len(hostRetryDelays)+1 rows.
func consecutiveHostUnavailableSteps(ctx context.Context, turns models.AgentTurnStore, scope models.Scope, runID string, through int64) (int, error) {
	rows, err := turns.ListAgentTurns(ctx, models.AgentTurnFilter{
		Scope: scope, RunID: runID, Descending: true, Limit: len(hostRetryDelays) + 2,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		if row == nil || row.Sequence > through {
			continue
		}
		if row.Status != models.AgentTurnStatusFailed || row.FailureCode != StepFailureHostUnavailable {
			break
		}
		count++
	}
	return count, nil
}

// hostRetryAvailableAt is when a Run requeued after its retries-th
// consecutive unavailable step may be claimed again.
func hostRetryAvailableAt(step *models.AgentTurn, retries int) *time.Time {
	if step == nil || step.CompletedAt == nil || retries < 1 || retries > len(hostRetryDelays) {
		return nil
	}
	at := step.CompletedAt.Add(hostRetryDelays[retries-1])
	return &at
}

// hostUnavailable finishes the step failed with host_unavailable and requeues
// the Run, or fails it once the retries are used up. Nothing the step
// produced applies, and it charges no Turn.
func (d *drive) hostUnavailable(ctx context.Context, st *stepState, cause error) stepResult {
	st.produced = false
	if err := d.finishStep(ctx, st, activity.FinishAgentTurnRequest{
		Status: models.AgentTurnStatusFailed, FailureCode: StepFailureHostUnavailable,
		Error: conversation.BoundedTurnFailureText(action.SanitizeActionError(cause, nil)),
	}); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	retries, err := consecutiveHostUnavailableSteps(ctx, d.l.store, d.run.Scope, d.run.ID, st.step.Sequence)
	if err != nil {
		return exitResultPtr(d.errorExit(err))
	}
	if retries <= len(hostRetryDelays) {
		return exitResult(d.requeue(ctx, st, hostRetryAvailableAt(st.step, retries), runRequeueReasonHostUnavailable,
			"The step host was unavailable; retrying shortly"))
	}
	return d.fail(ctx, st, agentrun.LoopFailure{
		Kind: conversation.RunFailureKindExecution, Code: StepFailureHostUnavailable, Message: hostUnavailableMessage,
	}, cause, StepFailureHostUnavailable)
}
