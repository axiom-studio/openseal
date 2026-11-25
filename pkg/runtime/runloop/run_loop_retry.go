package runloop

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// maximumOutputCutoffs is the bound of consecutive output cutoffs that kept
// nothing: the third fails the Run with provider_output_truncated.
const maximumOutputCutoffs = 3

// maximumResumedCutoffs bounds consecutive cut-off steps of any kind. A
// cutoff that kept text or tool calls is progress, so it does not count
// toward maximumOutputCutoffs; this keeps a model that never finishes a
// response from going on forever.
const maximumResumedCutoffs = 12

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
		data, err := json.Marshal(outputCutoffNoteData{OutputLimitTokens: failure.OutputLimitTokens, OutputTokens: failure.OutputTokens})
		if err != nil {
			return nil, err
		}
		note := &models.SessionEntry{
			ID: models.SessionEntryID(session.ID, key), Scope: d.run.Scope, SessionID: session.ID, RunID: d.run.ID,
			StepID: st.step.ID, Step: st.step.Sequence, Type: models.SessionEntryNote, SourceKey: key,
			Note: &models.NoteEntry{Kind: models.NoteOutputCutoff, Text: outputCutoffNoteText(failure.OutputTokens), Data: data},
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
	if failure.OutputTokens > 0 {
		payload["outputTokens"] = failure.OutputTokens
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
	OutputTokens      int64 `json:"outputTokens,omitempty"`
	// Kept marks a cutoff whose text and complete tool calls were kept.
	Kept bool `json:"kept,omitempty"`
}

// outputCutoffNoteText is what the model reads after a cutoff that kept
// nothing.
func outputCutoffNoteText(outputTokens int64) string {
	return "Output token limit hit" + cutOffSize(outputTokens) + ". Nothing of that response was kept and none of it ran. " +
		"Don't produce it again in one response: break the work into smaller pieces, one per response. " +
		"Write a long file in parts (create it with the first part, then add the rest in later responses), " +
		"or give a shorter answer and put the detail in a file."
}

// cutOffSize is " after about N tokens", or "" when the host did not say.
func cutOffSize(outputTokens int64) string {
	if outputTokens <= 0 {
		return ""
	}
	return fmt.Sprintf(" after about %d tokens", outputTokens)
}

// cutOff reports a step whose model response the provider cut off at its
// output limit with something usable in it (text or complete tool calls):
// the loop keeps that and has the model resume from it.
func (d *drive) cutOff(st *stepState) bool {
	return st.output != nil && st.output.StopReason == models.ModelStopMaxTokens &&
		st.mode == agentrun.StepModeNormal && d.session != nil
}

// resumeCutOff keeps a cut-off step and goes on to the next, as a person
// working in Claude Code would see it: the partial response stays, its
// complete tool calls run, and a note asks the model to resume. Text alone
// is never the final answer. done is false for a step with tool calls,
// which run as usual (finishAndSettle adds the note after their results).
func (d *drive) resumeCutOff(ctx context.Context, st *stepState) (stepResult, bool) {
	earlier, err := consecutiveCutOffSteps(ctx, d.l.store, d.run.Scope, d.run.ID, st.step.Sequence-1, maximumResumedCutoffs)
	if err != nil {
		return d.storeError(ctx, st, err, StepFailureProducerFailed), true
	}
	cutoffs := earlier + 1
	d.l.logger.Warnw("model output cut off; keeping it and resuming", "run", d.run.ID, "step", st.step.ID,
		"outputTokens", st.output.Usage.OutputTokens, "toolCalls", len(st.output.ToolCalls), "consecutiveCutoffs", cutoffs)
	if cutoffs >= maximumResumedCutoffs {
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: modelstep.TurnHostOutputTruncated,
			Message: fmt.Sprintf("The model's responses hit their output limit %d times in a row, so the run stopped.", cutoffs),
		}, nil, modelstep.TurnHostOutputTruncated), true
	}
	if len(st.output.ToolCalls) > 0 {
		return stepResult{}, false
	}
	if err := d.appendResumeNote(ctx, st); err != nil {
		return d.stepError(ctx, st, err, ""), true
	}
	return d.continueAfter(ctx, st, stepNext), true
}

// appendResumeNote adds the note after a kept cutoff. Its id is keyed by the
// step, so a step redone after a crash does not add it twice.
func (d *drive) appendResumeNote(ctx context.Context, st *stepState) error {
	_, err := d.appendStepEntries(ctx, func(session *models.AgentSession) ([]*models.SessionEntry, error) {
		key := models.OutputCutoffEntryKey(st.step.ID)
		data, err := json.Marshal(outputCutoffNoteData{OutputTokens: st.output.Usage.OutputTokens, Kept: true})
		if err != nil {
			return nil, err
		}
		note := &models.SessionEntry{
			ID: models.SessionEntryID(session.ID, key), Scope: d.run.Scope, SessionID: session.ID, RunID: d.run.ID,
			StepID: st.step.ID, Step: st.step.Sequence, Type: models.SessionEntryNote, SourceKey: key,
			Note: &models.NoteEntry{Kind: models.NoteOutputCutoff, Text: resumeNoteText(st.output.Usage.OutputTokens, len(st.output.ToolCalls) > 0), Data: data},
		}
		note.Tokens = modelstep.EstimateEntryTokens(note)
		return []*models.SessionEntry{note}, nil
	})
	return err
}

// resumeNoteText is what the model reads after a cutoff that was kept.
func resumeNoteText(outputTokens int64, ranTools bool) string {
	kept := "Your response so far was kept."
	if ranTools {
		kept = "Your response so far was kept and its complete tool calls ran."
	}
	return "Output token limit hit" + cutOffSize(outputTokens) + ". " + kept + " A tool call you had not finished was dropped and did not run. " +
		"Resume directly where you stopped: no apology, no recap. If you were writing your answer, continue it from the exact point " +
		"it was cut, mid-sentence if need be; what you write next is joined to it. Break the remaining work into smaller pieces."
}

// consecutiveCutOffSteps counts the steps through sequence through that
// ended at the output limit, kept or not, newest first, up to limit.
func consecutiveCutOffSteps(ctx context.Context, turns models.AgentTurnStore, scope models.Scope, runID string, through int64, limit int) (int, error) {
	rows, err := turns.ListAgentTurns(ctx, models.AgentTurnFilter{Scope: scope, RunID: runID, Descending: true, Limit: limit + 2})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		if row == nil || row.Sequence > through {
			continue
		}
		kept := row.Status == models.AgentTurnStatusCompleted && row.StopReason == string(models.ModelStopMaxTokens)
		lost := row.Status == models.AgentTurnStatusFailed && row.FailureCode == modelstep.TurnHostOutputTruncated
		if !kept && !lost {
			break
		}
		count++
	}
	return count, nil
}

// cutOffLead is the text of the steps right before this one that were cut
// off with text only: the start of an answer longer than one response.
func (d *drive) cutOffLead(ctx context.Context, st *stepState) (string, error) {
	entries, err := d.l.sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: d.run.Scope, RunID: d.run.ID, Types: []models.SessionEntryType{models.SessionEntryAssistant},
		Descending: true, Limit: maximumResumedCutoffs + 2,
	})
	if err != nil {
		return "", err
	}
	var parts []string
	for _, entry := range entries {
		if entry == nil || entry.Assistant == nil || entry.StepID == st.step.ID {
			continue
		}
		a := entry.Assistant
		if a.StopReason != models.ModelStopMaxTokens || len(a.ToolCalls) > 0 {
			break
		}
		parts = append(parts, a.Text)
	}
	slices.Reverse(parts)
	return strings.Join(parts, ""), nil
}
