package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The final chain (SPEC §3.3) and failure with explanation (SPEC §3.5).

const (
	finalSupersededText   = "superseded: new guidance arrived; read it and answer again"
	finalDeliveredStatus  = "delivered"
	finalCompletedSummary = "Run completed with its final answer"
	finalFailedSummary    = "Run failed with its final answer"
	finalExplainedSummary = "Run failed after the agent explained what went wrong"
	outputPublicationText = "Task output could not be saved. Resolve storage access and resume work to retry."
	finalEntryAttempts    = 4
)

// finishFinal runs the final chain for an accepted final answer: park
// without posting (pending setup), the D8 re-check, Deliver, output
// publication, then the guarded terminal transition. status is the terminal
// status ("" = completed; failed for a deterministic failed LoopFinal). In
// the explanation step the Run ends failed with its explained failure.
//
// An answer from a final-tool entry finishes the step first: a crash before
// the terminal transition resumes through that pending entry
// (ResumeOpenTools). A plain-text or deterministic final has no such entry,
// so its step stays running under its heartbeat through Deliver and
// publication, and FinishTurn completed comes just before the terminal
// transition (§7.2). A crash in between resumes the recorded step, which
// re-delivers idempotently instead of calling the model again (D14).
func (d *drive) finishFinal(ctx context.Context, st *stepState, final *FinalAnswer, status AgentRunStatus) stepResult {
	if final == nil {
		return d.stepError(ctx, st, errors.New("final chain without a final answer"), StepFailureProducerFailed)
	}
	if final.ToolEntryID != "" {
		if err := d.finishCompleted(ctx, st); err != nil {
			return exitResultPtr(d.writeErrorExit(err))
		}
	}
	explaining := st.mode == StepModeExplainFailure
	// 2. Pending setup: park without posting.
	if final.Park != nil && !final.Park.Deliver {
		return d.park(ctx, st, final.Park.Park)
	}
	run, err := d.reloadRun(ctx)
	if err != nil {
		return exitResultPtr(d.errorExit(err))
	}
	if run.Status != AgentRunStatusRunning || run.LeaseOwner != d.workerID {
		if !st.finished {
			if res, done := d.checkRunAfterProduce(ctx, st); done {
				return res
			}
		}
		return exitResultPtr(d.statusExit(run))
	}
	// D8: guidance the step did not see supersedes the answer.
	if len(unconsumedInterventions(run)) > 0 {
		return d.supersedeFinal(ctx, st, final)
	}
	count := len(run.PendingInterventions)
	// 3. Deliver.
	result := FinalDeliveryResult{Kind: FinalDeliveryComplete, Output: final.Output}
	if d.binding.Final != nil {
		delivered, err := d.binding.Final.Deliver(ctx, FinalDelivery{
			Run: cloneAgentRun(run), Step: cloneAgentTurn(st.current()), Answer: *final,
			Guard: ReplyRunGuard{
				Scope: run.Scope, RunID: run.ID, WorkerID: d.workerID,
				ConcurrencyKey: run.ConcurrencyKey, InterventionCount: count,
			},
		})
		switch {
		case errors.Is(err, ErrSteeringArrived):
			delivered = FinalDeliveryResult{Kind: FinalDeliveryContinue}
		case err != nil && isLeaseLoss(err):
			return exitResultPtr(d.writeErrorExit(err))
		case err != nil:
			return d.fail(ctx, st, LoopFailure{
				Kind: RunFailureKindExecution, Code: runFailureCodeFinalDelivery,
				Message: "The agent's reply could not be posted.",
			}, err, "")
		}
		result = delivered
	}
	switch result.Kind {
	case FinalDeliveryContinue:
		return d.supersedeFinal(ctx, st, final)
	case FinalDeliveryRetryFinal:
		return d.rejectDeliveredFinal(ctx, st, final, result.RejectionText)
	case FinalDeliveryFail:
		failure := LoopFailure{Kind: RunFailureKindExecution, Code: runFailureCodeFinalDelivery, Message: "The agent's reply could not be posted."}
		if result.Failure != nil {
			failure = *result.Failure
		}
		return d.fail(ctx, st, failure, nil, "")
	case FinalDeliveryPark:
		if final.Park == nil {
			return d.stepError(ctx, st, errors.New("final delivery parked without a final park"), StepFailureProducerFailed)
		}
	case FinalDeliveryComplete, "":
	default:
		return d.stepError(ctx, st, fmt.Errorf("unknown final delivery kind %q", result.Kind), StepFailureProducerFailed)
	}
	output := cloneMap(result.Output)
	if output == nil {
		output = cloneMap(final.Output)
	}
	// 4. Host extension fields of turn_final_answer.
	published, res, done := d.publishFinalOutput(ctx, st, final)
	if done {
		return res
	}
	for key, value := range published {
		if output == nil {
			output = map[string]interface{}{}
		}
		output[key] = value
	}
	// 5. The final tool entry is delivered.
	if err := d.closeFinalEntry(ctx, final, ToolEntryCompleted, &ToolResult{
		Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartJSON, Value: json.RawMessage(`{"status":"` + finalDeliveredStatus + `"}`)}},
	}); err != nil {
		return d.stepError(ctx, st, err, "")
	}
	if result.Kind == FinalDeliveryPark {
		return d.park(ctx, st, final.Park.Park)
	}
	// 6. FinishTurn completed (a tool final finished above), then the
	// guarded terminal transition, which also applies the step.
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	req := RunTransitionRequest{Status: AgentRunStatusCompleted, Output: output, Summary: finalCompletedSummary, RequireInterventionCount: &count}
	explained := false
	switch {
	case explaining && d.run.FailureExplanation != nil:
		req.Status = AgentRunStatusFailed
		req.Error = d.run.FailureExplanation.Message
		req.FailureExplanation = explainedRunFailure(d.run.FailureExplanation)
		req.Summary = finalExplainedSummary
		explained = true
	case status == AgentRunStatusFailed:
		// A deterministic failed final: its answer, when posted, is the
		// failure's explanation; otherwise terminal reporting posts the
		// kernel reply.
		message := strings.TrimSpace(final.Answer)
		explanation := beginRunFailureExplanation(d.run, RunFailureKindExecution, "execution_failed", message, "", "")
		if message != "" && d.binding.Final != nil {
			explanation = explainedRunFailure(explanation)
			explained = true
		}
		req.Status = AgentRunStatusFailed
		req.Error = explanation.Message
		req.FailureExplanation = explanation
		req.Summary = finalFailedSummary
	}
	run, err = d.commitRun(ctx, st, req, false)
	if errors.Is(err, ErrSteeringArrived) {
		// The reply is posted; a message that arrived after it is answered by
		// a follow-up Run (conversation_follow_up delivery).
		req.RequireInterventionCount = nil
		run, err = d.commitRun(ctx, st, req, false)
	}
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if explained && run.Status == AgentRunStatusFailed {
		// Failed after an explanation the user already has.
		return exitResult(RunLoopExit{Run: run, Reason: RunLoopExitFinal})
	}
	return exitResultPtr(d.statusExit(run))
}

// finishLoopFinal ends the Run with a deterministic producer's (or a
// preparer's early) final result through the final chain (§5.5 -> §7.2); a
// failed result is delivered too and ends the Run failed.
func (d *drive) finishLoopFinal(ctx context.Context, st *stepState, final *LoopFinal) stepResult {
	status := AgentRunStatus("")
	if final.Status == AgentRunStatusFailed {
		status = AgentRunStatusFailed
	}
	return d.finishFinal(ctx, st, &FinalAnswer{StepID: st.step.ID, Answer: final.Answer, Output: cloneMap(final.Output)}, status)
}

// supersedeFinal discards a final answer because new guidance arrived: the
// final tool entry gets an error result and the next step reads the guidance.
func (d *drive) supersedeFinal(ctx context.Context, st *stepState, final *FinalAnswer) stepResult {
	if err := d.closeFinalEntry(ctx, final, ToolEntryError, &ToolResult{
		Status: ToolResultSuperseded, IsError: true, Content: []ContentPart{{Type: ContentPartText, Text: finalSupersededText}},
	}); err != nil {
		return d.stepError(ctx, st, err, "")
	}
	return d.continueAfter(ctx, st, stepNext)
}

// rejectDeliveredFinal records a delivery-time rejection (retry_final): an
// error result on the final tool entry, or a final_rejection note for plain
// text. It counts toward maximumFinalRejections.
func (d *drive) rejectDeliveredFinal(ctx context.Context, st *stepState, final *FinalAnswer, text string) stepResult {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "The answer could not be delivered; correct it and answer again."
	}
	if final.ToolEntryID != "" {
		if err := d.closeFinalEntry(ctx, final, ToolEntryError, &ToolResult{
			Status: ToolResultFailed, IsError: true, Content: []ContentPart{{Type: ContentPartText, Text: text}},
		}); err != nil {
			return d.stepError(ctx, st, err, "")
		}
	} else if d.session != nil {
		stepID := final.StepID
		if stepID == "" {
			stepID = st.step.ID
		}
		_, err := d.appendStepEntries(ctx, func(session *AgentSession) ([]*SessionEntry, error) {
			key := FinalRejectionEntryKey(stepID)
			note := &SessionEntry{
				ID: SessionEntryID(session.ID, key), Scope: d.run.Scope, SessionID: session.ID, RunID: d.run.ID,
				StepID: stepID, Step: st.step.Sequence, Type: SessionEntryNote, SourceKey: key,
				Note: &NoteEntry{Kind: NoteFinalRejection, Text: text},
			}
			note.Tokens = estimateEntryTokens(note)
			return []*SessionEntry{note}, nil
		})
		if err != nil {
			return d.stepError(ctx, st, err, "")
		}
	}
	return d.continueAfter(ctx, st, stepFinalRejected)
}

// closeFinalEntry moves the final tool entry (if the answer came from one) to
// a terminal state. An entry already terminal is left as it is (replay).
func (d *drive) closeFinalEntry(ctx context.Context, final *FinalAnswer, to ToolEntryState, result *ToolResult) error {
	if final == nil || final.ToolEntryID == "" {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < finalEntryAttempts; attempt++ {
		entry, err := d.l.sessions.GetSessionEntry(ctx, d.run.Scope, final.ToolEntryID)
		if err != nil {
			return err
		}
		if entry.State.Terminal() {
			return nil
		}
		if !ValidToolEntryTransition(entry.State, to) {
			to = ToolEntryError
			result = &ToolResult{Status: ToolResultFailed, IsError: true, Content: result.Content}
		}
		now := d.l.now()
		_, err = d.l.sessions.TransitionToolEntry(ctx, ToolEntryTransitionRequest{
			Scope: d.run.Scope, EntryID: entry.ID, RunID: d.run.ID,
			Lease: AgentRunLeaseGuard{WorkerID: d.workerID, Now: now},
			From:  entry.State, ExpectedRevision: entry.Revision, To: to, Result: cloneToolResult(result), Now: now,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrToolEntryStateConflict) && !errors.Is(err, ErrRevisionConflict) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// publishFinalOutput publishes the host extension fields of the final tool
// call through the binding's FinalOutputPublisher. A failure pauses the Run
// (turn.output_publication_failed); done is then true.
func (d *drive) publishFinalOutput(ctx context.Context, st *stepState, final *FinalAnswer) (map[string]interface{}, stepResult, bool) {
	publisher := d.binding.FinalOutput
	if publisher == nil || final.ToolEntryID == "" {
		return nil, stepResult{}, false
	}
	properties := publisher.FinalAnswerProperties(cloneAgentRun(d.run))
	if len(properties) == 0 {
		return nil, stepResult{}, false
	}
	entry, err := d.l.sessions.GetSessionEntry(ctx, d.run.Scope, final.ToolEntryID)
	if err != nil {
		return nil, d.stepError(ctx, st, err, ""), true
	}
	fields := map[string]json.RawMessage{}
	if entry.Tool != nil && len(entry.Tool.Arguments) > 0 {
		var args map[string]json.RawMessage
		if err := json.Unmarshal(entry.Tool.Arguments, &args); err != nil {
			return nil, d.stepError(ctx, st, fmt.Errorf("decode final answer arguments: %w", err), ""), true
		}
		for key := range properties {
			if value, ok := args[key]; ok {
				fields[key] = append(json.RawMessage(nil), value...)
			}
		}
	}
	if len(fields) == 0 {
		return nil, stepResult{}, false
	}
	stepID := final.StepID
	if stepID == "" {
		stepID = entry.StepID
	}
	published, err := publisher.PublishFinalOutput(ctx, FinalOutputPublication{
		Run: cloneAgentRun(d.run), StepID: stepID, ToolCallID: final.ToolCallID, Fields: fields,
	})
	if err == nil {
		return published, stepResult{}, false
	}
	d.l.logger.Warnw("publish final output", "run", d.run.ID, "step", stepID, "error", err)
	if err := d.finishCompleted(ctx, st); err != nil {
		return nil, exitResultPtr(d.writeErrorExit(err)), true
	}
	run, err := d.commitRun(ctx, st, RunTransitionRequest{
		Status: AgentRunStatusPaused, Summary: outputPublicationText, Error: outputPublicationText,
		EventType: "turn.output_publication_failed", TurnID: stepID,
		Payload: map[string]interface{}{"code": runFailureCodeOutputPublication},
	}, false)
	if err != nil {
		return nil, exitResultPtr(d.writeErrorExit(err)), true
	}
	return nil, exitResultPtr(d.statusExit(run)), true
}

// fail is §3.5. The step (when still running) is finished failed. An
// explainable failure of a Run whose kind gets a model explanation records
// the failure with Explained=false and keeps the Run running: the next step
// is the tool-free explanation. Any other failure (including one during the
// explanation) ends the Run failed with Explained=false, and terminal
// reporting posts the kernel reply.
func (d *drive) fail(ctx context.Context, st *stepState, failure LoopFailure, cause error, stepCode string) stepResult {
	causeText := ""
	if cause != nil {
		causeText = boundedTurnFailureText(sanitizeActionError(cause, nil))
	}
	if stepCode == "" {
		stepCode = failure.Code
	}
	if st != nil && !st.finished {
		errText := causeText
		if errText == "" {
			errText = boundedTurnFailureText(failure.Message)
		}
		if err := d.finishStep(ctx, st, FinishAgentTurnRequest{
			Status: AgentTurnStatusFailed, FailureCode: runFailureCodeOrEmpty(stepCode), Error: errText,
		}); err != nil {
			return exitResultPtr(d.writeErrorExit(err))
		}
	}
	run := d.run
	explanation := beginRunFailureExplanation(run, failure.Kind, failure.Code, failure.Message, causeText, stepCode)
	explain := st != nil && run.FailureExplanation == nil && failure.Explainable && modelExplainsFailure(run) &&
		d.binding.Final != nil && d.session != nil && !noModelTurnFailureCode(failure.Code) && !noModelTurnFailureCode(stepCode)
	if explain {
		updated, err := d.commitRun(ctx, st, RunTransitionRequest{
			Status: AgentRunStatusRunning, FailureExplanation: explanation,
			Summary: "The agent will explain what went wrong",
		}, false)
		if err != nil {
			return exitResultPtr(d.writeErrorExit(err))
		}
		if updated.Status != AgentRunStatusRunning {
			return exitResultPtr(d.statusExit(updated))
		}
		return stepResult{kind: stepNext}
	}
	updated, err := d.commitRun(ctx, st, RunTransitionRequest{
		Status: AgentRunStatusFailed, Error: explanation.Message, FailureExplanation: explanation,
		Summary: "Run failed: " + explanation.Message,
	}, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	return exitResultPtr(d.statusExit(updated))
}
