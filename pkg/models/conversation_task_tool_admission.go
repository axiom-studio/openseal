package models

import (
	"encoding/json"
	"errors"
	"reflect"
)

// ---------------------------------------------------------------------------
// conversation_start_task admission (A4, section 04 §7.4), shared by every
// backend's CreateConversationTask.

// ConversationTaskStartedToolResult is the conversation_start_task tool result
// written by the admission transaction.
func ConversationTaskStartedToolResult(task *ConversationTask) *ToolResult {
	return ControlJSONResult(ToolResultSucceeded, map[string]interface{}{"status": "started", "taskId": task.ID,
		"workRunId": task.WorkRunID, "acknowledgment": task.Acknowledgment})
}

// validateConversationTaskToolRecord checks the tool transition and the work
// session of a create record before its transaction.
func validateConversationTaskToolRecord(record ConversationTaskCreateRecord) error {
	t, w, source := record.Task, record.WorkRun, record.SourceRun
	transition, session := record.ToolTransition, record.WorkSession
	if t.SourceToolEntryID == "" {
		if transition != nil || session != nil {
			return ErrInvalidConversationTask
		}
		return nil
	}
	if transition == nil || session == nil || !IsValidOpaqueIdentifier(t.SourceToolEntryID, 128) {
		return ErrInvalidConversationTask
	}
	if transition.Scope != t.Scope || transition.EntryID != t.SourceToolEntryID || transition.RunID != t.SourceRunID ||
		(transition.From != ToolEntryPending && transition.From != ToolEntryRunning) || transition.To != ToolEntryCompleted ||
		transition.Lease.WorkerID != record.WorkerID || transition.ActionCallID != "" ||
		!SameToolResult(transition.Result, ConversationTaskStartedToolResult(t)) {
		return ErrInvalidConversationTask
	}
	if err := ValidateToolEntryTransitionRequest(*transition); err != nil {
		return err
	}
	if err := ValidateSessionEvents(transition.Scope, transition.RunID, transition.Events, ErrInvalidToolEntryTransition); err != nil {
		return err
	}
	if session.Scope != t.Scope || session.OwnerRunID != w.ID || session.AgentID != w.AssignedAgentID ||
		session.ConversationID != t.ConversationID || session.ParentSessionID == "" || session.ParentSessionID != source.SessionID ||
		w.SessionID != RunAgentSessionID(t.Scope, w.ID) {
		return ErrInvalidConversationTask
	}
	return ValidateCreateRunAgentSessionTxRequest(*session)
}

// ConversationTaskSteeringSettled refuses an admission while the source Run
// has steering its loop has not read yet (section 04 §7.2 rule 2).
func ConversationTaskSteeringSettled(record ConversationTaskCreateRecord, current *AgentRun) error {
	if record.ToolTransition != nil && current != nil && current.ConsumedInterventions != len(current.PendingInterventions) {
		return ErrSteeringArrived
	}
	return nil
}

// VerifyConversationTaskToolEntry checks, inside the transaction, that the
// entry is the source step's conversation_start_task call in the chat session
// and that the work session forks just before that step's assistant entry.
func VerifyConversationTaskToolEntry(record ConversationTaskCreateRecord, current *AgentRun, entry, assistant *SessionEntry) error {
	if record.ToolTransition == nil {
		return nil
	}
	t := record.Task
	if entry == nil {
		return ErrSessionEntryNotFound
	}
	if entry.Type != SessionEntryTool || entry.Tool == nil || entry.Tool.Name != string(ControlToolStartTask) ||
		entry.RunID != current.ID || entry.SessionID != current.SessionID || entry.SessionID != record.WorkSession.ParentSessionID ||
		entry.StepID != t.SourceTurnID || entry.Step != t.SourceTurnNumber {
		return ErrConversationTaskConflict
	}
	if entry.State != record.ToolTransition.From {
		return ErrToolEntryStateConflict
	}
	if assistant == nil || assistant.Type != SessionEntryAssistant || assistant.SessionID != entry.SessionID ||
		assistant.StepID != entry.StepID || record.WorkSession.ForkSequence != assistant.Sequence-1 {
		return ErrConversationTaskConflict
	}
	return nil
}

// ConversationTaskToolReplayMatches accepts a replayed admission only when
// its tool entry already holds the same started result.
func ConversationTaskToolReplayMatches(record ConversationTaskCreateRecord, entry *SessionEntry) error {
	if entry == nil || entry.State != ToolEntryCompleted || entry.Tool == nil || !SameToolResult(entry.Tool.Result, record.ToolTransition.Result) {
		return ErrConversationTaskConflict
	}
	return nil
}

// SameToolResult compares results as JSON values: Postgres jsonb reorders the
// keys of a json content part.
func SameToolResult(a, b *ToolResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	var left, right interface{}
	for _, side := range []struct {
		result *ToolResult
		into   *interface{}
	}{{a, &left}, {b, &right}} {
		raw, err := json.Marshal(side.result)
		if err != nil || json.Unmarshal(raw, side.into) != nil {
			return false
		}
	}
	return reflect.DeepEqual(left, right)
}

// OptionalSessionEntry maps a missing entry to nil for the checks above.
func OptionalSessionEntry(entry *SessionEntry, err error) (*SessionEntry, error) {
	if errors.Is(err, ErrSessionEntryNotFound) {
		return nil, nil
	}
	return entry, err
}
