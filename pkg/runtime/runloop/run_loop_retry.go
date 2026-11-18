package runloop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Host retry (D13): a step whose host was unavailable is failed with
// FailureCode host_unavailable and the Run is requeued after a short backoff.
// The count of consecutive retries derives only from the durable step rows,
// so a worker restart schedules the same attempt; a completed step resets it.
//
// Output cutoff: a model call the provider cut off at its output cap is
// failed with FailureCode provider_output_truncated, a note asks the model to
// continue in smaller pieces, and the next step runs at once. It is counted
// the same way, from the step rows.

// hostRetryDelays are the waits before each requeued attempt. Once they are
// used up, the next unavailable host fails the Run.
var hostRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

const hostUnavailableMessage = "The agent's step host is unavailable, so the run stopped after retrying."

// consecutiveFailedSteps counts the newest step rows of the Run at or before
// sequence through, newest first, while each failed with code. It reads at
// most limit+1 rows, so it counts up to limit.
func consecutiveFailedSteps(ctx context.Context, turns models.AgentTurnStore, scope models.Scope, runID string, through int64, code string, limit int) (int, error) {
	rows, err := turns.ListAgentTurns(ctx, models.AgentTurnFilter{
		Scope: scope, RunID: runID, Descending: true, Limit: limit + 2,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		if row == nil || row.Sequence > through {
			continue
		}
		if row.Status != models.AgentTurnStatusFailed || row.FailureCode != code {
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
	retries, err := consecutiveFailedSteps(ctx, d.l.store, d.run.Scope, d.run.ID, st.step.Sequence, StepFailureHostUnavailable, len(hostRetryDelays)+1)
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

// maximumOutputCutoffs is the bound of consecutive output cutoffs: the third
// fails the Run with provider_output_truncated.
const maximumOutputCutoffs = 3

// outputCutoff handles a model call cut off at its output cap. The cut-off
// response is discarded: the host returns no result for it, and its tool
// calls would be truncated JSON anyway. The step is failed, a note tells the
// model what happened, and the Run keeps going. The third consecutive cutoff
// fails the Run the ordinary way.
func (d *drive) outputCutoff(ctx context.Context, st *stepState, failure *modelstep.TurnHostFailure, cause error) stepResult {
	earlier, err := consecutiveFailedSteps(ctx, d.l.store, d.run.Scope, d.run.ID, st.step.Sequence-1, modelstep.TurnHostOutputTruncated, maximumOutputCutoffs)
	if err != nil {
		return d.storeError(ctx, st, err, StepFailureProducerFailed)
	}
	cutoffs := earlier + 1
	if cutoffs >= maximumOutputCutoffs {
		return d.stepError(ctx, st, cause, StepFailureProducerFailed)
	}
	// The note goes in before the step finishes, so a finished cutoff step
	// always has it; a crash in between redoes the model call with the note.
	_, err = d.appendStepEntries(ctx, func(session *models.AgentSession) ([]*models.SessionEntry, error) {
		key := models.OutputCutoffEntryKey(st.step.ID)
		data, err := json.Marshal(outputCutoffNoteData{OutputLimitTokens: failure.OutputLimitTokens})
		if err != nil {
			return nil, err
		}
		note := &models.SessionEntry{
			ID: models.SessionEntryID(session.ID, key), Scope: d.run.Scope, SessionID: session.ID, RunID: d.run.ID,
			StepID: st.step.ID, Step: st.step.Sequence, Type: models.SessionEntryNote, SourceKey: key,
			Note: &models.NoteEntry{Kind: models.NoteOutputCutoff, Text: outputCutoffNoteText(failure.OutputLimitTokens), Data: data},
		}
		note.Tokens = modelstep.EstimateEntryTokens(note)
		return []*models.SessionEntry{note}, nil
	})
	if err != nil {
		return d.stepError(ctx, st, err, "")
	}
	if err := d.finishStep(ctx, st, activity.FinishAgentTurnRequest{
		Status: models.AgentTurnStatusFailed, FailureCode: modelstep.TurnHostOutputTruncated,
		Error: conversation.BoundedTurnFailureText(action.SanitizeActionError(cause, nil)),
	}); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	d.l.logger.Warnw("model output cut off; continuing with a note", "run", d.run.ID, "step", st.step.ID,
		"outputLimitTokens", failure.OutputLimitTokens, "consecutiveCutoffs", cutoffs, "error", cause)
	payload := map[string]interface{}{"reason": modelstep.TurnHostOutputTruncated, "consecutiveCutoffs": cutoffs}
	if failure.OutputLimitTokens > 0 {
		payload["outputLimitTokens"] = failure.OutputLimitTokens
	}
	run, err := d.commitRun(ctx, st, activity.RunTransitionRequest{
		Status: models.AgentRunStatusRunning, EventType: runEventOutputCutoff, Severity: models.ActivitySeverityWarning,
		Summary: "The model's response hit its output limit and was discarded; the agent continues in smaller pieces",
		Payload: payload,
	}, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if run.Status != models.AgentRunStatusRunning {
		return exitResultPtr(d.statusExit(run))
	}
	return stepResult{kind: stepNext}
}

// runEventOutputCutoff is the activity event of a step discarded by an
// output cutoff.
const runEventOutputCutoff = "run.output_cutoff"

// outputCutoffNoteData is the kernel data of an output_cutoff note.
type outputCutoffNoteData struct {
	OutputLimitTokens int64 `json:"outputLimitTokens,omitempty"`
}

// outputCutoffNoteText is what the model reads after a cutoff.
func outputCutoffNoteText(limitTokens int64) string {
	limit := "the output limit"
	if limitTokens > 0 {
		limit = fmt.Sprintf("the output limit of %d tokens", limitTokens)
	}
	return "Your last response hit " + limit + " and was cut off, so it was discarded and none of it ran. " +
		"Don't try to produce it again in one response. Continue from where you are and split long work into smaller pieces: " +
		"write a long file in parts (create it with the first part, then append the rest in later steps), " +
		"or give a shorter answer and put the detail in a file."
}
