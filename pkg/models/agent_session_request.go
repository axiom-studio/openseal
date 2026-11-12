package models

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateAppendAgentSessionEntries checks an append request before the
// transaction. In-transaction checks (lease, session binding, replay, source
// keys, compaction cut against the path) are the backend's.
func ValidateAppendAgentSessionEntries(req AppendAgentSessionEntriesRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSessionEntry, err)
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.RunID) == "" {
		return InvalidSessionEntryf("append needs sessionId and runId")
	}
	if len(req.Entries) > MaxSessionAppendEntries {
		return entryTooLarge("append has %d entries (max %d)", len(req.Entries), MaxSessionAppendEntries)
	}
	if len(req.Entries) == 0 && req.ConsumeInterventionsThrough == nil && req.ExpectedChannelWatermark == nil {
		return InvalidSessionEntryf("an empty append needs a cursor or a watermark")
	}
	if req.ConsumeInterventionsThrough != nil && *req.ConsumeInterventionsThrough < 0 {
		return InvalidSessionEntryf("negative intervention cursor")
	}
	if req.ExpectedChannelWatermark != nil && (*req.ExpectedChannelWatermark < 0 || req.ChannelWatermark < *req.ExpectedChannelWatermark) {
		return InvalidSessionEntryf("channel watermark must not move backwards")
	}
	if req.Lease == nil {
		if len(req.Entries) == 0 || req.ConsumeInterventionsThrough != nil || req.ExpectedChannelWatermark != nil {
			return InvalidSessionEntryf("a lease-less append holds only notes")
		}
		for _, entry := range req.Entries {
			if entry == nil || entry.Type != SessionEntryNote {
				return InvalidSessionEntryf("a lease-less append holds only notes")
			}
		}
	} else if strings.TrimSpace(req.Lease.WorkerID) == "" {
		return InvalidSessionEntryf("lease needs a worker id")
	}

	ids := make(map[string]bool, len(req.Entries))
	sourceKeys := make(map[string]bool)
	// assistant call ids by step, and the tool entries seen for each
	type stepCalls struct {
		calls map[string]bool
		seen  map[string]bool
	}
	steps := make(map[string]*stepCalls)
	// tool call ids of Directive (deterministic) tool entries by step
	directiveSteps := make(map[string]map[string]bool)
	total := 0
	for i, entry := range req.Entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if (entry.Scope != Scope{}) && entry.Scope != req.Scope {
			return InvalidSessionEntryf("entry %s belongs to another scope", entry.ID)
		}
		if entry.SessionID != "" && entry.SessionID != req.SessionID {
			return InvalidSessionEntryf("entry %s belongs to another session", entry.ID)
		}
		if entry.RunID != "" && entry.RunID != req.RunID {
			return InvalidSessionEntryf("entry %s belongs to another run", entry.ID)
		}
		if ids[entry.ID] {
			return InvalidSessionEntryf("duplicate entry id %s in batch", entry.ID)
		}
		ids[entry.ID] = true
		if entry.SourceKey != "" {
			if sourceKeys[entry.SourceKey] {
				return InvalidSessionEntryf("duplicate source key %s in batch", entry.SourceKey)
			}
			sourceKeys[entry.SourceKey] = true
		}
		switch entry.Type {
		case SessionEntryAssistant:
			if _, dup := steps[entry.StepID]; dup {
				return InvalidSessionEntryf("two assistant entries for step %s in batch", entry.StepID)
			}
			if _, mixed := directiveSteps[entry.StepID]; mixed {
				return InvalidSessionEntryf("step %s has both directive tool entries and an assistant entry", entry.StepID)
			}
			calls := &stepCalls{calls: map[string]bool{}, seen: map[string]bool{}}
			for _, call := range entry.Assistant.ToolCalls {
				calls.calls[call.ID] = true
			}
			steps[entry.StepID] = calls
		case SessionEntryTool:
			switch entry.State {
			case ToolEntryPending, ToolEntryCompleted, ToolEntryError:
			default:
				return InvalidSessionEntryf("tool entry %s is appended as pending, completed or error, not %s", entry.ID, entry.State)
			}
			if len(entry.Tool.Directive) > 0 {
				// Deterministic producer: its step has no assistant entry.
				if _, mixed := steps[entry.StepID]; mixed {
					return InvalidSessionEntryf("step %s has both directive tool entries and an assistant entry", entry.StepID)
				}
				seen := directiveSteps[entry.StepID]
				if seen == nil {
					seen = make(map[string]bool)
					directiveSteps[entry.StepID] = seen
				}
				if seen[entry.ToolCallID] {
					return InvalidSessionEntryf("tool call %s has two tool entries", entry.ToolCallID)
				}
				seen[entry.ToolCallID] = true
				break
			}
			calls := steps[entry.StepID]
			if calls == nil || !calls.calls[entry.ToolCallID] {
				return InvalidSessionEntryf("tool entry %s has no assistant tool call %s earlier in the batch", entry.ID, entry.ToolCallID)
			}
			if calls.seen[entry.ToolCallID] {
				return InvalidSessionEntryf("tool call %s has two tool entries", entry.ToolCallID)
			}
			calls.seen[entry.ToolCallID] = true
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return InvalidSessionEntryf("entry %d does not encode: %v", i, err)
		}
		total += len(encoded)
	}
	if total > MaxSessionAppendBytes {
		return entryTooLarge("append is %d bytes (max %d)", total, MaxSessionAppendBytes)
	}
	for stepID, calls := range steps {
		for callID := range calls.calls {
			if !calls.seen[callID] {
				return InvalidSessionEntryf("tool call %s of step %s has no tool entry in the batch", callID, stepID)
			}
		}
	}
	return nil
}

// StampAppendedSessionEntry returns the stored form of a validated request
// entry: envelope fields filled, sequence assigned, revision 1, tokens
// estimated when 0. Terminal tool entries get CompletedAt.
func StampAppendedSessionEntry(in *SessionEntry, req AppendAgentSessionEntriesRequest, sequence int64) *SessionEntry {
	out := CloneSessionEntry(in)
	out.Scope = req.Scope
	out.SessionID = req.SessionID
	out.RunID = req.RunID
	out.Sequence = sequence
	out.Revision = 1
	out.CreatedAt = req.Now
	out.UpdatedAt = req.Now
	if out.Tokens == 0 {
		out.Tokens = EstimateSessionEntryTokens(out)
	}
	if out.Tool != nil && out.State.Terminal() && out.Tool.CompletedAt == nil {
		now := req.Now
		out.Tool.CompletedAt = &now
	}
	return out
}

// ValidateToolEntryTransitionRequest checks the request alone (edge table,
// wait/result rules) before any I/O.
func ValidateToolEntryTransitionRequest(req ToolEntryTransitionRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidToolEntryTransition, err)
	}
	if strings.TrimSpace(req.EntryID) == "" || strings.TrimSpace(req.RunID) == "" {
		return fmt.Errorf("%w: entryId and runId are required", ErrInvalidToolEntryTransition)
	}
	if strings.TrimSpace(req.Lease.WorkerID) == "" && req.To != ToolEntryError && req.To != ToolEntryInterrupted {
		return fmt.Errorf("%w: lease needs a worker id", ErrInvalidToolEntryTransition)
	}
	if !ValidToolEntryTransition(req.From, req.To) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidToolEntryTransition, req.From, req.To)
	}
	if req.ExpectedRevision < 0 || req.Tokens < 0 {
		return fmt.Errorf("%w: negative revision or tokens", ErrInvalidToolEntryTransition)
	}
	if (req.Wait != nil) != req.To.Awaiting() {
		return fmt.Errorf("%w: wait is required exactly for awaiting states", ErrInvalidToolEntryTransition)
	}
	if (req.Result != nil) != req.To.Terminal() {
		return fmt.Errorf("%w: result is required exactly for terminal states", ErrInvalidToolEntryTransition)
	}
	if req.Result != nil {
		if err := validateToolResultForState(req.To, req.Result, ErrInvalidToolEntryTransition); err != nil {
			return err
		}
	}
	return nil
}

// ApplyToolEntryTransition returns the entry after req, or the fence/CAS error.
// The lease is the backend's to check; the result is validated (caps).
func ApplyToolEntryTransition(current *SessionEntry, req ToolEntryTransitionRequest) (*SessionEntry, error) {
	if current == nil {
		return nil, ErrSessionEntryNotFound
	}
	if current.Type != SessionEntryTool || current.Tool == nil {
		return nil, fmt.Errorf("%w: entry %s is not a tool entry", ErrInvalidToolEntryTransition, current.ID)
	}
	if current.RunID != req.RunID {
		return nil, ErrToolEntryNotOwned
	}
	if current.State != req.From || (req.ExpectedRevision != 0 && current.Revision != req.ExpectedRevision) {
		return nil, ErrToolEntryStateConflict
	}
	if req.ActionCallID != "" && current.ActionCallID != "" && current.ActionCallID != req.ActionCallID {
		return nil, fmt.Errorf("%w: entry %s already has action call %s", ErrInvalidToolEntryTransition, current.ID, current.ActionCallID)
	}
	next := CloneSessionEntry(current)
	next.State = req.To
	if req.ActionCallID != "" {
		next.ActionCallID = req.ActionCallID
	}
	next.Tool.Wait = cloneToolEntryWait(req.Wait)
	next.Tool.Result = CloneToolResult(req.Result)
	now := req.Now
	if req.To == ToolEntryRunning {
		next.Tool.StartedAt = &now
	}
	if req.To.Terminal() {
		completed := now
		next.Tool.CompletedAt = &completed
	}
	if patch := req.Kernel; patch != nil {
		if patch.ResultDigest != "" {
			next.Tool.ResultDigest = patch.ResultDigest
		}
		if patch.OriginRef != "" {
			next.Tool.OriginRef = patch.OriginRef
		}
		if patch.DuplicateOfEntryID != "" {
			next.Tool.DuplicateOfEntryID = patch.DuplicateOfEntryID
		}
		if patch.Parked != nil {
			next.Tool.Parked = *patch.Parked
		}
		if patch.Guard != nil {
			next.Tool.Guard = cloneToolGuardOutcome(patch.Guard)
		}
		if patch.Control != nil {
			next.Tool.Control = CloneRawMessage(patch.Control)
		}
		if patch.SurfacedTools != nil {
			next.Tool.SurfacedTools = CloneSurfacedTools(patch.SurfacedTools)
		}
		if patch.LoadedInstructions != nil {
			next.Tool.LoadedInstructions = cloneStrings(patch.LoadedInstructions)
		}
		if patch.PinnedRun != nil {
			next.Tool.PinnedRun = CloneRawMessage(patch.PinnedRun)
		}
	}
	next.Revision = current.Revision + 1
	next.UpdatedAt = now
	switch {
	case req.Tokens > 0:
		next.Tokens = req.Tokens
	case req.Result != nil || req.Kernel != nil:
		next.Tokens = EstimateSessionEntryTokens(next)
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// NormalizeSessionEntryFilter validates a list filter and applies the limit defaults.
func NormalizeSessionEntryFilter(filter SessionEntryFilter) (SessionEntryFilter, error) {
	if err := filter.Scope.Validate(); err != nil {
		return filter, fmt.Errorf("%w: %v", ErrInvalidSessionEntry, err)
	}
	if filter.SessionID == "" && filter.RunID == "" && filter.ActionCallID == "" && filter.StepID == "" {
		return filter, InvalidSessionEntryf("list needs sessionId, runId, actionCallId or stepId")
	}
	if filter.SessionID == "" && (filter.AfterSequence != 0 || filter.AfterCompaction) {
		return filter, InvalidSessionEntryf("afterSequence and afterCompaction need sessionId")
	}
	if filter.AfterSequence < 0 || filter.Limit < 0 {
		return filter, InvalidSessionEntryf("negative afterSequence or limit")
	}
	for _, kind := range filter.Types {
		if !kind.Valid() {
			return filter, InvalidSessionEntryf("unknown type %q", kind)
		}
	}
	for _, state := range filter.States {
		if !state.Valid() {
			return filter, InvalidSessionEntryf("unknown state %q", state)
		}
	}
	if filter.Limit == 0 {
		filter.Limit = defaultSessionEntryListLimit
	}
	if filter.Limit > MaxSessionEntryListLimit {
		filter.Limit = MaxSessionEntryListLimit
	}
	return filter, nil
}

// SessionEntryMatchesFilter is the row predicate of a normalized filter, minus
// AfterCompaction, ordering and Limit (the backend applies those).
func SessionEntryMatchesFilter(e *SessionEntry, filter SessionEntryFilter) bool {
	if e == nil || e.Scope != filter.Scope {
		return false
	}
	if filter.SessionID != "" && e.SessionID != filter.SessionID {
		return false
	}
	if filter.RunID != "" && e.RunID != filter.RunID {
		return false
	}
	if filter.ActionCallID != "" && e.ActionCallID != filter.ActionCallID {
		return false
	}
	if filter.StepID != "" && e.StepID != filter.StepID {
		return false
	}
	if e.Sequence <= filter.AfterSequence {
		return false
	}
	if len(filter.Types) > 0 && !containsEntryType(filter.Types, e.Type) {
		return false
	}
	if len(filter.States) > 0 || len(filter.ToolNames) > 0 {
		if e.Type != SessionEntryTool || e.Tool == nil {
			return false
		}
		if len(filter.States) > 0 && !containsToolState(filter.States, e.State) {
			return false
		}
		if len(filter.ToolNames) > 0 && !MatchesStringFilter(filter.ToolNames, e.Tool.Name) {
			return false
		}
	}
	if len(filter.NoteKinds) > 0 {
		if e.Type != SessionEntryNote || e.Note == nil || !containsNoteKind(filter.NoteKinds, e.Note.Kind) {
			return false
		}
	}
	return true
}

func containsEntryType(list []SessionEntryType, v SessionEntryType) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsToolState(list []ToolEntryState, v ToolEntryState) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsNoteKind(list []NoteKind, v NoteKind) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
