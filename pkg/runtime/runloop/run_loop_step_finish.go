package runloop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// finishAndSettle is phases 9-11 after an inline phase that did not end the
// step: finish + apply, then the single settle-phase call.
func (d *drive) finishAndSettle(ctx context.Context, st *stepState) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	run, err := d.commitRun(ctx, st, activity.RunTransitionRequest{Status: models.AgentRunStatusRunning}, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if run.Status != models.AgentRunStatusRunning {
		return exitResultPtr(d.statusExit(run))
	}
	if st.settle {
		st.settle = false
		out, err := d.binding.Tools.SettleStep(ctx, d.toolsInput(st, st.entries))
		if res, done := d.toolOutcome(ctx, st, out, err); done {
			return res
		}
	}
	if d.finalRejected(ctx, st) {
		return stepResult{kind: stepFinalRejected}
	}
	return stepResult{kind: stepNext}
}

// finalRejected reports whether this step's final-tool entry ended in an
// error result (a rejection counted toward maximumFinalRejections).
func (d *drive) finalRejected(ctx context.Context, st *stepState) bool {
	if st.finalEntry == "" {
		return false
	}
	entry, err := d.l.sessions.GetSessionEntry(ctx, d.run.Scope, st.finalEntry)
	return err == nil && entry != nil && entry.State == models.ToolEntryError
}

// continueAfter applies the step (if it is not yet) with the Run running and
// returns kind.
func (d *drive) continueAfter(ctx context.Context, st *stepState, kind stepResultKind) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if st.applied {
		return stepResult{kind: kind}
	}
	run, err := d.commitRun(ctx, st, activity.RunTransitionRequest{Status: models.AgentRunStatusRunning}, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if run.Status != models.AgentRunStatusRunning {
		return exitResultPtr(d.statusExit(run))
	}
	return stepResult{kind: kind}
}

// endedByTool finishes the step of a Run a tool ended in its own transaction.
func (d *drive) endedByTool(ctx context.Context, st *stepState, run *models.AgentRun) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil && !errors.Is(err, models.ErrTurnLeaseHeld) && !errors.Is(err, models.ErrRevisionConflict) {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if run != nil {
		d.run = models.CloneAgentRun(run)
	}
	return exitResultPtr(d.statusExit(d.run))
}

// budgetExhausted pauses the Run on an executor's budget exit.
func (d *drive) budgetExhausted(ctx context.Context, st *stepState, b *agentrun.BudgetExhaustion) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	dimension := ""
	if len(b.Dimensions) > 0 {
		dimension = b.Dimensions[0]
	}
	pause := stepBudgetPause(d.run, d.workerID, b.Summary, b.Admission, dimension)
	if b.ActionCallID != "" {
		pause.Payload["actionCallId"] = b.ActionCallID
	}
	run, err := d.commitRun(ctx, st, pause, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	return exitResultPtr(d.statusExit(run))
}

// park is §3.2. A committed park (fork, run_operation, delegate) already
// moved the Run; the loop only finishes the step. Every other park is a CAS
// transition on the revision read after the step's last write, guarded by
// the intervention count; a conflict re-plans instead of re-applying.
func (d *drive) park(ctx context.Context, st *stepState, park agentrun.LoopPark) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	for replans := 0; ; replans++ {
		if park.Committed {
			run, err := d.reloadRun(ctx)
			if err != nil {
				return exitResultPtr(d.errorExit(err))
			}
			return exitResultPtr(d.statusExit(run))
		}
		count := len(d.run.PendingInterventions)
		summary := park.Summary
		if summary == "" {
			summary = fmt.Sprintf("Run is %s", strings.ReplaceAll(string(park.Status), "_", " "))
		}
		run, err := d.commitRun(ctx, st, activity.RunTransitionRequest{
			Status: park.Status, WakeCondition: park.Wake, Summary: summary, RequireInterventionCount: &count,
		}, true)
		if err == nil {
			return exitResultPtr(d.statusExit(run))
		}
		if !errors.Is(err, models.ErrRevisionConflict) && !errors.Is(err, models.ErrSteeringArrived) {
			return exitResultPtr(d.writeErrorExit(err))
		}
		if replans+1 >= maximumParkReplans {
			return exitResult(d.requeue(ctx, st, nil, runRequeueReasonParkReplan, "Run requeued after its park was re-planned too often"))
		}
		reloaded, err := d.reloadRun(ctx)
		if err != nil {
			return exitResultPtr(d.errorExit(err))
		}
		if reloaded.Status != models.AgentRunStatusRunning || reloaded.LeaseOwner != d.workerID {
			return exitResultPtr(d.statusExit(reloaded))
		}
		var next *agentrun.LoopPark
		if d.binding.Tools != nil {
			next, err = d.binding.Tools.ReplanPark(ctx, d.toolsInput(st, st.entries))
			if err != nil {
				return d.stepError(ctx, st, err, "")
			}
		}
		if next == nil {
			// The awaited decision or card is already settled: continue.
			return d.continueAfter(ctx, st, stepNext)
		}
		park = *next
	}
}

// finishStep finishes the running step (phase 9 and every exit). The
// heartbeat stops first, so the finish holds the latest revision.
func (d *drive) finishStep(ctx context.Context, st *stepState, req activity.FinishAgentTurnRequest) error {
	if st.finished {
		return nil
	}
	st.lease.stopHeartbeat()
	if st.lease.lost.Load() {
		return models.ErrTurnLeaseHeld
	}
	if req.Usage == (models.TurnUsage{}) {
		req.Usage = st.usage
	}
	req.Usage.InputTokens += st.compaction.InputTokens
	req.Usage.OutputTokens += st.compaction.OutputTokens
	req.Usage.CacheReadTokens += st.compaction.CacheReadTokens
	req.Usage.CacheWriteTokens += st.compaction.CacheWriteTokens
	req.Usage.Cost += st.compaction.Cost
	req.Usage.NativeOperations += st.hostCalls
	req.WorkerID = d.workerID
	finished, err := st.lease.write(func(turn *models.AgentTurn) (*models.AgentTurn, error) {
		req.ExpectedRevision = turn.Revision
		return d.l.turns.FinishTurn(context.WithoutCancel(ctx), turn.Scope, turn.ID, req)
	})
	if err != nil {
		return err
	}
	st.step = finished
	st.finished = true
	return nil
}

// addCompaction adds the usage of one compaction call to the step.
func (st *stepState) addCompaction(u models.ModelStepUsage) {
	st.compaction.InputTokens += int(u.InputTokens)
	st.compaction.OutputTokens += int(u.OutputTokens)
	st.compaction.CacheReadTokens += int(u.CacheReadTokens)
	st.compaction.CacheWriteTokens += int(u.CacheWriteTokens)
	if u.Cost > 0 {
		st.compaction.Cost += u.Cost
	}
}

func (d *drive) finishCompleted(ctx context.Context, st *stepState) error {
	if st.usage.DurationMS == 0 {
		st.usage.DurationMS = d.l.now().Sub(st.started).Milliseconds()
	}
	return d.finishStep(ctx, st, activity.FinishAgentTurnRequest{Status: models.AgentTurnStatusCompleted})
}

// stepError classifies an error of a step phase. defaultCode is the step
// FailureCode of an unclassified error ("" = execution_failed).
func (d *drive) stepError(ctx context.Context, st *stepState, err error, defaultCode string) stepResult {
	var loopFailure *modelstep.LoopFailureError
	var hostFailure *modelstep.TurnHostFailure
	switch {
	case errors.Is(err, errRunEnded):
		if res, done := d.checkRunAfterProduce(ctx, st); done {
			return res
		}
		return exitResultPtr(d.writeErrorExit(err))
	case isLeaseLoss(err) || st.lease.lost.Load() || ctx.Err() != nil:
		if res, done := d.checkRunAfterProduce(ctx, st); done {
			return res
		}
		return exitResult(RunLoopExit{Run: d.run, Reason: RunLoopExitLeaseLost, Err: err})
	case st.durCtx != nil && stepDurationExpired(st.stepCtx, st.durCtx):
		return d.durationExpired(ctx, st, d.l.now().Sub(st.started).Milliseconds())
	case errors.Is(err, modelstep.ErrTurnHostUnavailable):
		return d.hostUnavailable(ctx, st, err)
	case errors.As(err, &loopFailure):
		return d.fail(ctx, st, loopFailure.Failure, err, loopFailure.Failure.Code)
	case errors.As(err, &hostFailure):
		code := conversation.RunFailureCodeOrEmpty(hostFailure.Code)
		if code == "" {
			code = StepFailureProducerFailed
		}
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: code, Message: hostFailure.Error(),
			Explainable: conversation.TurnFailureCanBeExplained(err, code),
		}, err, code)
	case errors.Is(err, modelstep.ErrTurnHostConfiguration):
		message := "The agent's step host is not configured correctly."
		if errors.Is(err, models.ErrInvalidModelStepRequest) {
			// The host is fine; the step built from the session was refused.
			message = "The agent's next step could not be prepared from this conversation."
		}
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: StepFailureHostConfiguration,
			Message: message,
		}, err, StepFailureHostConfiguration)
	}
	code := defaultCode
	if code == "" {
		code = "execution_failed"
	}
	return d.fail(ctx, st, agentrun.LoopFailure{
		Kind: conversation.RunFailureKindExecution, Code: code, Message: "The requested work did not complete.",
		Explainable: conversation.TurnFailureCanBeExplained(err, code),
	}, err, code)
}

// storeError handles an error of a store read or of the budget reservation
// in phases 1-3. A kernel-classified error (lease loss, a host or loop
// failure, an exhausted budget, a missing or oversized session) goes through
// stepError. Any other error is infrastructure: the Run is left running, the
// step lease is released, and the Run's lease expiry lets a later claim
// resume it.
func (d *drive) storeError(ctx context.Context, st *stepState, err error, defaultCode string) stepResult {
	var loopFailure *modelstep.LoopFailureError
	var hostFailure *modelstep.TurnHostFailure
	switch {
	case errors.Is(err, errRunEnded), isLeaseLoss(err), st.lease.lost.Load(), ctx.Err() != nil,
		st.durCtx != nil && stepDurationExpired(st.stepCtx, st.durCtx),
		errors.Is(err, modelstep.ErrTurnHostUnavailable), errors.Is(err, modelstep.ErrTurnHostConfiguration),
		errors.As(err, &loopFailure), errors.As(err, &hostFailure),
		errors.Is(err, models.ErrAgentSessionNotFound), errors.Is(err, models.ErrAgentSessionPathTooLarge), errors.Is(err, models.ErrAgentSessionForkTooDeep):
		return d.stepError(ctx, st, err, defaultCode)
	case errors.Is(err, models.ErrBudgetExhausted):
		return d.stepError(ctx, st, err, StepFailureBudgetExhausted)
	}
	d.l.logger.Warnw("step store error; the run is left for lease recovery", "run", d.run.ID, "step", st.step.ID, "error", err)
	return exitResultPtr(d.errorExit(err))
}

// stepFenced checks that the step is still this worker's running step and the
// Run still this claim's. Observers call it from host and executor
// goroutines, so it takes the ids captured when the step began and never
// reads the loop's d.run or st.
func (d *drive) stepFenced(ctx context.Context, scope models.Scope, runID, stepID string) error {
	current, err := d.l.store.GetAgentTurn(ctx, scope, stepID)
	if err != nil {
		return err
	}
	if current == nil || current.Status != models.AgentTurnStatusRunning || current.LeaseOwner != d.workerID ||
		current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(d.l.now()) {
		return models.ErrTurnLeaseHeld
	}
	active, err := d.l.store.GetAgentRun(ctx, scope, runID)
	if err != nil {
		return err
	}
	if active == nil || active.Status != models.AgentRunStatusRunning || active.LeaseOwner != "" && active.LeaseOwner != d.workerID {
		return models.ErrLeaseLost
	}
	return nil
}
