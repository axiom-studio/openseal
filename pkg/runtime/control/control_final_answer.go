package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The final answer of a Run is its plain reply: an assistant entry with no
// tool calls (section 04 §11, SPEC §3.3 step 1). This file turns that text
// into the FinalAnswer the loop delivers, or refuses it. A refusal is a
// final_rejection note and the loop continues; the loop owns the bound of 3
// consecutive rejections (A9) and the delivery. Nothing reviews what the
// answer says: the checks only refuse a reply that cannot be delivered at
// all. Decision Runs finish with request_decision instead.
//
// Order: mode → empty (silent, reactions, routine rule) → finalAnswerPark.

// Final answer error codes (stable; the model sees them in the rejection).
const (
	finalSilentAfterChangeCode  = "silent_after_change"
	finalToolRequiredCode       = "final_tool_required"
	finalEmptyReplyCode         = "empty_answer"
	finalPlainTextAttemptsLimit = 3 // the 3rd plain-text attempt where a final tool is required fails the Run
	finalEmptyReplyAttemptLimit = 3 // the 3rd empty plain-text reply fails the Run
)

// finalAnswerRejection is a check's verdict: a rejection the model can
// correct (Failure nil), or a failure of the Run (Failure set; the rejection
// is still recorded on the entry or as the note).
type finalAnswerRejection struct {
	Code    string
	Message string
	Extra   map[string]interface{}
	Failure *agentrun.LoopFailure
}

// rejectFinal records a rejection: for a plain reply a final_rejection note,
// for a final tool call (request_decision) an error result on its entry
// (with EntryMeta.ErrorCode). A rejection with a Failure then fails the Run.
func (x *Executor) rejectFinal(ctx context.Context, cc agentrun.ControlContext, plainText bool, r *finalAnswerRejection) (Result, error) {
	var recorded *models.SessionEntry
	if plainText {
		note, err := x.appendFinalRejectionNote(ctx, cc, r)
		if err != nil {
			return Result{}, err
		}
		recorded = note
	} else {
		entry, err := x.transitionControlEntry(ctx, cc, models.ToolEntryError, nil,
			ErrorResult(models.ToolResultFailed, r.Code, r.Message, r.Extra), &EntryMeta{ErrorCode: r.Code})
		if err != nil {
			return Result{}, err
		}
		recorded = entry
	}
	if r.Failure != nil {
		return Result{Kind: Failed, Run: cc.Run, Entry: recorded, Failure: r.Failure}, nil
	}
	return Result{Kind: Continue, Run: cc.Run, Entry: recorded}, nil
}

// finalRejectionNoteData is the kernel data of a final_rejection note.
type finalRejectionNoteData struct {
	ErrorCode string `json:"errorCode"`
}

// appendFinalRejectionNote appends the step's final_rejection note (one per
// step: id and SourceKey final-rejection:<stepId>).
func (x *Executor) appendFinalRejectionNote(ctx context.Context, cc agentrun.ControlContext, r *finalAnswerRejection) (*models.SessionEntry, error) {
	code, message := r.Code, r.Message
	sessionID := cc.Run.SessionID
	if cc.Session != nil {
		sessionID = cc.Session.ID
	}
	if sessionID == "" || cc.Step == nil || cc.Step.ID == "" {
		return nil, errors.New("plain-text final needs the Run's session and step")
	}
	data, err := json.Marshal(finalRejectionNoteData{ErrorCode: code})
	if err != nil {
		return nil, err
	}
	key := models.FinalRejectionEntryKey(cc.Step.ID)
	note := &models.SessionEntry{
		ID: models.SessionEntryID(sessionID, key), Scope: cc.Run.Scope, SessionID: sessionID, RunID: cc.Run.ID,
		StepID: cc.Step.ID, Step: cc.Step.Sequence, Type: models.SessionEntryNote, SourceKey: key,
		Note: &models.NoteEntry{Kind: models.NoteFinalRejection, Text: message, Data: data},
	}
	now := x.now()
	lease := cc.LeaseGuard(now)
	appended, err := x.Sessions.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: cc.Run.Scope, SessionID: sessionID, RunID: cc.Run.ID, Lease: &lease,
		Entries: []*models.SessionEntry{note}, Now: now,
	})
	if err != nil {
		return nil, err
	}
	if len(appended.Entries) == 1 {
		return appended.Entries[0], nil
	}
	// Replayed or skipped by SourceKey: the note of this step already exists.
	return x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, note.ID)
}

// priorFinalErrorCodes counts the rejection codes of this Run's earlier plain
// replies (final_rejection notes); the current step's note is not counted.
// The plain-text attempt limits use it.
func (x *Executor) priorFinalErrorCodes(ctx context.Context, cc agentrun.ControlContext) (map[string]int, error) {
	counts := map[string]int{}
	notes, err := x.Sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID, NoteKinds: []models.NoteKind{models.NoteFinalRejection}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, entry := range notes {
		// The current step's own note (a replay after a crash) is not prior.
		if entry.Note == nil || len(entry.Note.Data) == 0 || cc.Step != nil && entry.StepID == cc.Step.ID {
			continue
		}
		var data finalRejectionNoteData
		if err := json.Unmarshal(entry.Note.Data, &data); err != nil {
			return nil, fmt.Errorf("decode final rejection note %s: %w", entry.ID, err)
		}
		if data.ErrorCode != "" {
			counts[data.ErrorCode]++
		}
	}
	return counts, nil
}

// runReacted: a react call of this Run completed (reactions-only reply).
func (x *Executor) runReacted(ctx context.Context, cc agentrun.ControlContext) (bool, error) {
	entries, err := x.Sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID, ToolNames: []string{string(models.ControlToolReact)},
		States: []models.ToolEntryState{models.ToolEntryCompleted}, Limit: 1,
	})
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// runChangedSomethingStatuses are the action call states in which the call
// ran: compensation undoes a change that still happened.
var runChangedSomethingStatuses = []models.ActionCallStatus{
	models.ActionCallStatusSucceeded, models.ActionCallStatusCompensating, models.ActionCallStatusCompensated,
}

// runChangedSomething reports whether the Run, or any Run under the same
// root (a delegated or forked child), completed an action whose side effect
// is not none or read, granted or approved. It runs only for a silent routine
// final, never on a step.
func (x *Executor) runChangedSomething(ctx context.Context, cc agentrun.ControlContext) (bool, error) {
	actions, ok := x.Portfolio.(models.ActionStore)
	if !ok {
		return false, errors.New("control executor portfolio is not an action store; a silent routine final needs the Run's calls")
	}
	const pageSize = 200
	rootID := strings.TrimSpace(cc.Run.RootRunID)
	if rootID == "" {
		rootID = cc.Run.ID
	}
	runIDs := []string{cc.Run.ID}
	for offset := 0; ; offset += pageSize {
		runs, err := x.Portfolio.ListAgentRuns(ctx, models.AgentRunFilter{Scope: cc.Run.Scope, RootRunID: rootID, Limit: pageSize, Offset: offset})
		if err != nil {
			return false, err
		}
		for _, run := range runs {
			if run != nil && run.ID != cc.Run.ID {
				runIDs = append(runIDs, run.ID)
			}
		}
		if len(runs) < pageSize {
			break
		}
	}
	for _, runID := range runIDs {
		for offset := 0; ; offset += pageSize {
			calls, err := actions.ListActionCalls(ctx, models.ActionFilter{
				Scope: cc.Run.Scope, RunID: runID, Status: runChangedSomethingStatuses, Limit: pageSize, Offset: offset,
			})
			if err != nil {
				return false, err
			}
			for _, call := range calls {
				if call != nil && call.SideEffect != skill.SideEffectNone && call.SideEffect != skill.SideEffectRead {
					return true, nil
				}
			}
			if len(calls) < pageSize {
				break
			}
		}
	}
	return false, nil
}

// finalPark applies finalAnswerPark to the Run's governed calls on its path.
func (x *Executor) finalPark(ctx context.Context, cc agentrun.ControlContext, answer string) (*agentrun.FinalPark, error) {
	path := cc.Path
	if path == nil {
		sessionID := cc.Run.SessionID
		if cc.Session != nil {
			sessionID = cc.Session.ID
		}
		if sessionID == "" {
			return nil, nil
		}
		loaded, err := x.Sessions.LoadAgentSessionPath(ctx, cc.Run.Scope, sessionID)
		if err != nil {
			return nil, err
		}
		path = loaded
	}
	governed := path.Compaction != nil && path.Compaction.Compaction != nil && len(path.Compaction.Compaction.ActionCallIDs) > 0
	for _, entry := range path.Entries {
		if entry != nil && entry.RunID == cc.Run.ID && entry.ActionCallID != "" {
			governed = true
			break
		}
	}
	if !governed {
		return nil, nil
	}
	actions, ok := x.Portfolio.(models.ActionStore)
	if !ok {
		return nil, errors.New("control executor portfolio is not an action store; the final answer park needs the Run's calls")
	}
	views, err := LoadGovernedToolViews(ctx, actions, cc.Run.Scope, path, cc.Run.ID)
	if err != nil {
		return nil, err
	}
	plan := finalAnswerPark(cc.Run, views, cc.Authority.CanAskUser, answer, cc.Run.PendingInterventions)
	if plan == nil {
		return nil, nil
	}
	return &agentrun.FinalPark{Deliver: plan.Deliver, Park: agentrun.LoopPark{Status: plan.Status, Wake: plan.Wake, Summary: plan.Summary}}, nil
}

// ---------------------------------------------------------------------------
// Plain-text final (§11.2)

// PlainTextFinal handles an assistant entry with no tool calls (SPEC §3.1
// phase 8). cc.Entry is nil; cc.Assistant is that entry and cc.Step its step
// (still running). Results:
//   - Final: an accepted final answer (the text, or silent / reactions only
//     for an empty reply where that is allowed);
//   - Continue: a final_rejection note was appended (Entry is the note); the
//     loop counts it toward the rejection bound;
//   - Failed: the 3rd plain reply where a final tool is required, or the 3rd
//     empty reply.
func (x *Executor) PlainTextFinal(ctx context.Context, cc agentrun.ControlContext, text string) (Result, error) {
	if x == nil || x.Sessions == nil {
		return Result{}, errors.New("control executor has no session store")
	}
	if cc.Run == nil || cc.Step == nil {
		return Result{}, errors.New("plain-text final needs its Run and step")
	}
	if cc.Run.Status != models.AgentRunStatusRunning || cc.WorkerID == "" || cc.Run.LeaseOwner != cc.WorkerID {
		return Result{}, models.ErrLeaseLost
	}
	if cc.Assistant != nil && cc.Assistant.Assistant != nil && len(cc.Assistant.Assistant.ToolCalls) > 0 {
		return Result{}, errors.New("plain-text final on an assistant entry with tool calls")
	}
	cc.Entry = nil
	mode := cc.Authority.EffectiveFinalMode()
	// 1. Modes that need a structured final tool.
	if tool := finalToolRequired(mode); tool != "" {
		prior, err := x.priorFinalErrorCodes(ctx, cc)
		if err != nil {
			return Result{}, err
		}
		message := fmt.Sprintf("Finish with %s (%s); a plain reply is not accepted in this run.", tool, x.registry().finalToolRequiredFields(tool, cc.Authority))
		rejection := &finalAnswerRejection{Code: finalToolRequiredCode, Message: message}
		if prior[finalToolRequiredCode]+1 >= finalPlainTextAttemptsLimit {
			rejection.Failure = &agentrun.LoopFailure{Kind: conversation.RunFailureKindExecution, Code: finalToolRequiredCode,
				Message: fmt.Sprintf("The agent replied without %s %d times, so the run stopped.", tool, finalPlainTextAttemptsLimit)}
		}
		return x.rejectFinal(ctx, cc, true, rejection)
	}
	answer := strings.TrimSpace(text)
	output := map[string]interface{}{conversationrun.FinalOutputSummaryKey: answer}
	// 2. An empty reply: silent when spoken, the reactions when the Run
	// reacted, nothing to report for a routine occurrence that changed
	// nothing; otherwise refused.
	if answer == "" {
		empty, rejection, err := x.emptyReply(ctx, cc, mode)
		if err != nil || rejection != nil {
			if err != nil {
				return Result{}, err
			}
			return x.rejectFinal(ctx, cc, true, rejection)
		}
		output = empty
	}
	final := &agentrun.FinalAnswer{PlainText: true, StepID: cc.Step.ID, Answer: answer, Output: output}
	// 3. OTP wait or pending setup (section 07 §8).
	park, err := x.finalPark(ctx, cc, answer)
	if err != nil {
		return Result{}, err
	}
	final.Park = park
	return Result{Kind: Final, Run: cc.Run, Final: final}, nil
}

// emptyReply is the output of an empty reply that is allowed (silent or
// reactions only), or the rejection of one that is not.
func (x *Executor) emptyReply(ctx context.Context, cc agentrun.ControlContext, mode agentrun.FinalAnswerMode) (map[string]interface{}, *finalAnswerRejection, error) {
	if mode == agentrun.FinalModeSpoken {
		return map[string]interface{}{agentrun.FinalOutputSilentKey: true}, nil, nil
	}
	reacted, err := x.runReacted(ctx, cc)
	if err != nil {
		return nil, nil, err
	}
	if reacted {
		return map[string]interface{}{agentrun.FinalOutputReactedKey: true}, nil, nil
	}
	if mode == agentrun.FinalModeDefault && cc.Authority.RoutineOccurrence {
		// A routine occurrence that changed something always reports it.
		changed, err := x.runChangedSomething(ctx, cc)
		if err != nil {
			return nil, nil, err
		}
		if !changed {
			return map[string]interface{}{agentrun.FinalOutputSilentKey: true}, nil, nil
		}
		return nil, &finalAnswerRejection{Code: finalSilentAfterChangeCode, Message: "This run changed something, so report what it did."}, nil
	}
	prior, err := x.priorFinalErrorCodes(ctx, cc)
	if err != nil {
		return nil, nil, err
	}
	rejection := &finalAnswerRejection{Code: finalEmptyReplyCode, Message: "An empty reply is not an answer. Write the reply."}
	if prior[finalEmptyReplyCode]+1 >= finalEmptyReplyAttemptLimit {
		rejection.Failure = &agentrun.LoopFailure{Kind: conversation.RunFailureKindExecution, Code: finalEmptyReplyCode,
			Message: "The agent sent an empty reply three times, so the run stopped."}
	}
	return nil, rejection, nil
}

// finalToolRequired names the final tool a plain reply cannot replace in
// mode ("" when plain text is a final answer).
func finalToolRequired(mode agentrun.FinalAnswerMode) models.ControlToolName {
	if mode == agentrun.FinalModeDecision {
		return models.ControlToolRequestDecision
	}
	return ""
}

func (r Registry) finalToolRequiredFields(tool models.ControlToolName, a agentrun.ControlAuthority) string {
	h, ok := r[tool]
	if !ok {
		return "its required fields"
	}
	required, _ := h.Spec(a).InputSchema["required"].([]string)
	if len(required) == 0 {
		return "its required fields"
	}
	return strings.Join(required, ", ")
}
