package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Terminal close (loop-16): once a Run is terminal, nothing of it may stay
// open. Its running step is finished canceled/run_ended and every open tool
// entry it owns gets an error result, because a chat session that later Runs
// keep using must never hold an assistant tool call without a tool result.
// It runs after the terminal Run transition, from CascadeTerminalRun, which
// both CommandAgentRun and the worker's terminal finalization call. It is
// idempotent, so a crash between the transition and the close is repaired
// by the next cascade.

// StepFailureRunEnded is the step FailureCode of a step finished because its
// Run became terminal.
const StepFailureRunEnded = "run_ended"

const (
	// terminalCloseWorkerID owns a step that had no live lease while the
	// terminal close finishes it.
	terminalCloseWorkerID  = "run-terminal-close"
	terminalCloseStepLease = time.Minute
	terminalCloseStepError = "The run ended before this step finished."
	terminalCloseAttempts  = 4
	terminalClosePageSize  = 500
)

// terminalRunToolResultText is the tool result of an open tool entry of a
// terminal Run.
func terminalRunToolResultText(status AgentRunStatus) string {
	return fmt.Sprintf("Not run: the request was %s", status)
}

func terminalRunToolResult(status AgentRunStatus) *ToolResult {
	return &ToolResult{
		Status: ToolResultCanceled, IsError: true,
		Content: []ContentPart{{Type: ContentPartText, Text: terminalRunToolResultText(status)}},
	}
}

// closeTerminalRunWork finishes the running steps and closes the open tool
// entries of a terminal Run. Stores without steps or a session log have
// nothing to close.
func closeTerminalRunWork(ctx context.Context, store interface{}, run *AgentRun, now time.Time) error {
	if run == nil || !isTerminalAgentRunStatus(run.Status) {
		return nil
	}
	var errs []error
	if turns, ok := store.(AgentTurnStore); ok {
		errs = append(errs, closeTerminalRunSteps(ctx, turns, run, now))
	}
	if sessions, ok := store.(AgentSessionStore); ok {
		errs = append(errs, closeTerminalRunToolEntries(ctx, sessions, run, now))
	}
	return errors.Join(errs...)
}

// closeTerminalRunSteps finishes every running step of the Run as
// canceled/run_ended. A live step lease is written under its owner, so the
// loop's own later finish fails its revision CAS instead of overwriting it.
func closeTerminalRunSteps(ctx context.Context, turns AgentTurnStore, run *AgentRun, now time.Time) error {
	steps, err := turns.ListAgentTurns(ctx, AgentTurnFilter{
		Scope: run.Scope, RunID: run.ID, Statuses: []AgentTurnStatus{AgentTurnStatusRunning}, Limit: terminalClosePageSize,
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, step := range steps {
		if step != nil {
			errs = append(errs, finishTerminalRunStep(ctx, turns, step.Scope, step.ID, now))
		}
	}
	return errors.Join(errs...)
}

func finishTerminalRunStep(ctx context.Context, turns AgentTurnStore, scope Scope, stepID string, now time.Time) error {
	var lastErr error
	for attempt := 0; attempt < terminalCloseAttempts; attempt++ {
		step, err := turns.GetAgentTurn(ctx, scope, stepID)
		if err != nil {
			return err
		}
		if step == nil || step.Status != AgentTurnStatusRunning {
			return nil
		}
		owner := step.LeaseOwner
		if owner == "" || step.LeaseExpiresAt == nil || !step.LeaseExpiresAt.After(now) {
			claimed, err := turns.ClaimAgentTurn(ctx, scope, stepID, terminalCloseWorkerID, now, terminalCloseStepLease)
			if err != nil {
				lastErr = err
				continue
			}
			step, owner = claimed, terminalCloseWorkerID
		}
		expected := step.Revision
		finished := cloneAgentTurn(step)
		finished.Status = AgentTurnStatusCanceled
		finished.FailureCode = StepFailureRunEnded
		finished.Error = terminalCloseStepError
		finished.LeaseOwner = ""
		finished.LeaseExpiresAt = nil
		completed := now
		finished.CompletedAt = &completed
		finished.UpdatedAt = now
		finished.Revision = expected + 1
		err = turns.UpdateAgentTurn(ctx, finished, expected, owner)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrTurnLeaseHeld) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("finish step %s of an ended run: %w", stepID, lastErr)
}

// closeTerminalRunToolEntries gives every open tool entry of the Run an error
// result. The transition is lease-less: the store accepts it only while the
// Run is terminal. An entry that moved meanwhile is listed again.
func closeTerminalRunToolEntries(ctx context.Context, sessions AgentSessionStore, run *AgentRun, now time.Time) error {
	result := terminalRunToolResult(run.Status)
	for stalled := 0; ; {
		open, err := sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
			Scope: run.Scope, RunID: run.ID, States: OpenToolEntryStates, Limit: maxSessionEntryListLimit,
		})
		if err != nil {
			return err
		}
		if len(open) == 0 {
			return nil
		}
		closed := 0
		var errs []error
		for _, entry := range open {
			_, err := sessions.TransitionToolEntry(ctx, ToolEntryTransitionRequest{
				Scope: run.Scope, EntryID: entry.ID, RunID: run.ID,
				From: entry.State, ExpectedRevision: entry.Revision, To: ToolEntryError,
				Result: cloneToolResult(result), Now: now,
			})
			switch {
			case err == nil:
				closed++
			case errors.Is(err, ErrToolEntryStateConflict):
			default:
				errs = append(errs, fmt.Errorf("close tool entry %s: %w", entry.ID, err))
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		if closed > 0 {
			stalled = 0
		} else if stalled++; stalled >= terminalCloseAttempts {
			return fmt.Errorf("close tool entries of run %s: %d entries kept moving", run.ID, len(open))
		}
	}
}
