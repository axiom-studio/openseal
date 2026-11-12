package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// Claim reconciliation and crash resume (D14, SPEC §3.7, section 02 §8).
//
// Before the first step of every claim the loop reads the step rows this Run
// has not applied yet:
//
//   - a finished row (a park settled at this claim, or a crash between
//     FinishTurn and applyStep) is applied now, with the Run running. A
//     plain-text final row runs its final chain again instead (resumePlainFinal),
//     so the model is not asked again;
//   - a running row (at most one) is claimed. Its dead owner's live step
//     lease requeues the Run at the lease expiry. A claimed row whose
//     assistant entry exists resumes after the model call (resumeRecordedStep:
//     backfill RecordStepModel, open tools, the final chain); without one the
//     step redoes its model call on the same row (same step id, so the budget
//     reservation keyed by it is reused).
//
// Open tool entries of earlier steps are settled by phase 2 of the next step
// (Tools.ResumeOpenTools), which owns the per-tool resume table.

// resumedStep is a running step row of this Run left by an earlier claim and
// now claimed by this worker.
type resumedStep struct {
	step      *models.AgentTurn
	assistant *models.SessionEntry // nil when the model output was never recorded
}

// reconcileTurnPage bounds the unapplied step rows one claim reads. A Run
// has at most one running row and normally at most one finished unapplied
// row (the step a park or a crash left).
const reconcileTurnPage = 8

// reconcile applies finished but unapplied step rows and claims a running
// one. A running row whose dead owner's lease is still live requeues the Run
// at its expiry.
func (d *drive) reconcile(ctx context.Context) (*resumedStep, *RunLoopExit) {
	rows, err := d.l.turns.ListTurns(ctx, models.AgentTurnFilter{
		Scope: d.run.Scope, RunID: d.run.ID, AfterSequence: d.run.LastAppliedTurn, Limit: reconcileTurnPage,
	})
	if err != nil {
		return nil, d.errorExit(err)
	}
	var resumed *resumedStep
	for _, row := range rows {
		if row == nil || row.Sequence <= d.run.LastAppliedTurn {
			continue
		}
		if models.IsTerminalAgentTurnStatus(row.Status) {
			if exit, handled := d.resumePlainFinal(ctx, row); handled {
				if exit != nil {
					return nil, exit
				}
				continue
			}
			st := &stepState{step: row, finished: true, produced: stepRowProduced(row)}
			run, err := d.commitRun(ctx, st, activity.RunTransitionRequest{Status: models.AgentRunStatusRunning}, false)
			if err != nil {
				return nil, d.writeErrorExit(err)
			}
			if run.Status != models.AgentRunStatusRunning {
				return nil, d.statusExit(run)
			}
			continue
		}
		if resumed != nil {
			return nil, d.errorExit(fmt.Errorf("run %s has more than one running step", d.run.ID))
		}
		claimed, err := d.l.turns.ClaimTurn(ctx, row.Scope, row.ID, d.workerID, d.l.config.StepLeaseDuration)
		if errors.Is(err, models.ErrTurnLeaseHeld) {
			exit := d.requeue(ctx, nil, row.LeaseExpiresAt, runRequeueReasonStepLeaseHeld, "Run requeued until its previous step lease expires")
			return nil, &exit
		}
		if err != nil {
			return nil, d.writeErrorExit(err)
		}
		resumed = &resumedStep{step: claimed}
		assistant, err := d.stepAssistantEntry(ctx, claimed.ID)
		if err != nil {
			return nil, d.errorExit(err)
		}
		resumed.assistant = assistant
	}
	return resumed, nil
}

// stepAssistantEntry is the assistant entry of a step in the current
// session, or nil when the step never recorded its model output.
func (d *drive) stepAssistantEntry(ctx context.Context, stepID string) (*models.SessionEntry, error) {
	if d.session == nil {
		return nil, nil
	}
	entry, err := d.l.sessions.GetSessionEntry(ctx, d.run.Scope, models.SessionEntryID(d.session.ID, models.AssistantEntryKey(stepID)))
	switch {
	case errors.Is(err, models.ErrSessionEntryNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case entry == nil || entry.Type != models.SessionEntryAssistant || entry.Assistant == nil:
		return nil, nil
	}
	return entry, nil
}

// stepRowProduced reports whether a finished step row ran its producer, for
// the Turns budget dimension (D12).
func stepRowProduced(step *models.AgentTurn) bool {
	return step != nil && (step.AssistantEntryID != "" || step.ToolCallCount > 0 || step.Final || step.StopReason != "")
}

// resumePlainFinal finishes the final chain of a completed, unapplied
// plain-text final step: a crash between its FinishTurn and its terminal
// transition (the plain-text chain finishes the row just before that
// transition, after Deliver). The chain runs again instead of a new model
// call; Deliver replays the posted reply by its key. handled is false when
// the row is applied the ordinary way; with handled true, a nil exit means
// the step was applied and the Run keeps running.
//
// A rejected plain final (its final-rejection note exists) is applied the
// ordinary way. Final-tool answers need nothing here: their step is applied
// before the settle phase, a pending final entry is settled by the next
// step's phase 2 (ResumeOpenTools re-runs §3.3 from step 1), and a reply
// already posted is replayed by its delivery key.
func (d *drive) resumePlainFinal(ctx context.Context, row *models.AgentTurn) (*RunLoopExit, bool) {
	if row.Status != models.AgentTurnStatusCompleted || !row.Final || row.ToolCallCount > 0 || d.session == nil {
		return nil, false
	}
	assistant, err := d.stepAssistantEntry(ctx, row.ID)
	if err != nil {
		return d.errorExit(err), true
	}
	if assistant == nil {
		return nil, false
	}
	rejection, err := d.l.sessions.GetSessionEntry(ctx, d.run.Scope, models.SessionEntryID(d.session.ID, models.FinalRejectionEntryKey(row.ID)))
	switch {
	case err == nil && rejection != nil:
		return nil, false
	case err != nil && !errors.Is(err, models.ErrSessionEntryNotFound):
		return d.errorExit(err), true
	}
	st := d.finishedStepState(ctx, row)
	st.assistant, st.output = assistant, assistant.Assistant
	res := d.plainTextFinal(ctx, st, assistant.Assistant.Text)
	if res.kind == stepExit {
		return &res.exit, true
	}
	if !st.applied {
		// The chain continued without applying (a re-check that left the
		// Run running): apply the step like any finished row.
		run, err := d.commitRun(ctx, st, activity.RunTransitionRequest{Status: models.AgentRunStatusRunning}, false)
		if err != nil {
			return d.writeErrorExit(err), true
		}
		if run.Status != models.AgentRunStatusRunning {
			return d.statusExit(run), true
		}
	}
	return nil, true
}

// finishedStepState is the loop's view of a finished step an earlier claim
// left unapplied: the final chain and the Run writes treat it as finished.
func (d *drive) finishedStepState(ctx context.Context, row *models.AgentTurn) *stepState {
	mode := agentrun.StepModeNormal
	if conversation.RunExplainingFailure(d.run) {
		mode = agentrun.StepModeExplainFailure
	}
	return &stepState{
		lease: &stepLease{turn: row}, step: row, mode: mode, started: d.l.now(),
		stepCtx: ctx, durCtx: ctx, finished: true, produced: stepRowProduced(row), usage: row.Usage,
	}
}
