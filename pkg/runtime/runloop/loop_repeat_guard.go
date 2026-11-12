package runloop

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// D9 repeat detection (section 08 §5.4, SPEC §5.4). The executor builds a
// repeatLedger from the Run's session entries every time it starts a step's
// tools, evaluates every call of the step before dispatching any, and asks
// for notes after each executed call. The ledger is never persisted: it is
// derived from the log, so counts survive claims, parks and crashes (D14),
// and it reads by RunID, so compaction does not affect it.

// ToolGuardOutcome.Kind values set by the repeat guard. repeat_refused,
// duplicate_suppressed and repeat_limit reuse the refused action_calls codes
// (action_loop_refusal.go); the two below create no action_calls row.
const (
	ToolGuardSameStepDuplicate = "same_step_duplicate"
	ToolGuardRunStopping       = "run_stopping"
)

// repeatLimitCount is the position of the identical call that fails the Run.
const repeatLimitCount = 5

type repeatVerdictKind int

const (
	repeatRun           repeatVerdictKind = iota // dispatch as usual
	repeatRefuse                                 // not run: repeat_refused, same_step_duplicate or run_stopping
	repeatSuppressWrite                          // not run: duplicate_suppressed, with the prior result
	repeatFailRun                                // not run: repeat_limit, the Run fails after the step
)

type repeatVerdict struct {
	Kind repeatVerdictKind
	// Code is the ToolGuardOutcome.Kind for every verdict but repeatRun.
	Code string
	// Prior is the counted prior entry (texts, suppressed result,
	// DuplicateOfEntryID); for same_step_duplicate the earlier call of the
	// step; nil for run_stopping.
	Prior *models.SessionEntry
	// Count is this call's position in the counted sequence (3 = 3rd
	// identical); 0 for same_step_duplicate and run_stopping.
	Count int
}

// repeatLedger holds one Run's tool and user entries in sequence order.
type repeatLedger struct {
	entries []*models.SessionEntry
	byID    map[string]int
}

func newRepeatLedger() *repeatLedger {
	return &repeatLedger{byID: map[string]int{}}
}

// loadRepeatLedger pages the Run's tool and user entries of sessionID until
// exhausted.
func loadRepeatLedger(ctx context.Context, store conversation.LoopSessionEntryLister, scope models.Scope, sessionID, runID string) (*repeatLedger, error) {
	if store == nil || sessionID == "" || runID == "" {
		return nil, errors.New("repeat ledger needs a session store, session and Run")
	}
	ledger := newRepeatLedger()
	after := int64(0)
	for {
		page, err := store.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
			Scope: scope, SessionID: sessionID, RunID: runID,
			Types:         []models.SessionEntryType{models.SessionEntryTool, models.SessionEntryUser},
			AfterSequence: after, Limit: models.MaxSessionEntryListLimit,
		})
		if err != nil {
			return nil, fmt.Errorf("load repeat ledger: %w", err)
		}
		for _, entry := range page {
			ledger.observe(entry)
		}
		if len(page) < models.MaxSessionEntryListLimit {
			return ledger, nil
		}
		next := page[len(page)-1].Sequence
		if next <= after {
			return nil, errors.New("load repeat ledger: session entries are not in sequence order")
		}
		after = next
	}
}

// observe records an appended entry or the new revision of a transitioned
// one. Only tool and user entries matter; others are ignored.
func (l *repeatLedger) observe(entry *models.SessionEntry) {
	if l == nil || entry == nil || (entry.Type != models.SessionEntryTool && entry.Type != models.SessionEntryUser) {
		return
	}
	if l.byID == nil {
		l.byID = map[string]int{}
	}
	if index, ok := l.byID[entry.ID]; ok && entry.ID != "" {
		l.entries[index] = entry
		return
	}
	position := sort.Search(len(l.entries), func(i int) bool { return l.entries[i].Sequence > entry.Sequence })
	l.entries = append(l.entries, nil)
	copy(l.entries[position+1:], l.entries[position:])
	l.entries[position] = entry
	for i := position; i < len(l.entries); i++ {
		if id := l.entries[i].ID; id != "" {
			l.byID[id] = i
		}
	}
}

func repeatEntryEffect(entry *models.SessionEntry) RepeatEffect {
	if entry == nil || entry.Tool == nil {
		return ""
	}
	return RepeatEffect(entry.Tool.RepeatEffect)
}

func repeatGuardKind(entry *models.SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Guard == nil {
		return ""
	}
	return entry.Tool.Guard.Kind
}

// repeatCounted: terminal, not interrupted, and not refused as a same-step
// duplicate or a stopping sibling.
func repeatCounted(entry *models.SessionEntry) bool {
	if entry == nil || entry.Type != models.SessionEntryTool || entry.Tool == nil || entry.Tool.ArgumentsDigest == "" {
		return false
	}
	if !entry.State.Terminal() || entry.State == models.ToolEntryInterrupted {
		return false
	}
	if entry.Tool.Result != nil && entry.Tool.Result.Status == models.ToolResultInterrupted {
		return false
	}
	switch repeatGuardKind(entry) {
	case ToolGuardSameStepDuplicate, ToolGuardRunStopping:
		return false
	}
	return true
}

// repeatExecuted: counted and not refused by any guard.
func repeatExecuted(entry *models.SessionEntry) bool {
	return repeatCounted(entry) && entry.Tool.Guard == nil
}

func repeatRefused(entry *models.SessionEntry) bool {
	switch repeatGuardKind(entry) {
	case action.RefusedLoopCallRepeat, action.RefusedLoopCallDuplicateSuppressed:
		return repeatCounted(entry)
	}
	return false
}

// repeatEntryResultDigest is the stored ResultDigest, or the digest of the
// stored result when the executor did not set one.
func repeatEntryResultDigest(entry *models.SessionEntry) string {
	if entry.Tool.ResultDigest != "" {
		return entry.Tool.ResultDigest
	}
	return modelstep.ToolResultDigest(entry.Tool.Result)
}

// repeatStreak walks the counted entries of key in sequence order up to and
// including sequence through (0 = all) and returns the streak of identical
// results with no reset point between, the last executed entry and the
// executed entry before it. trailing applies the reset points after the last
// counted entry (the "now" check before a new call).
func (l *repeatLedger) repeatStreak(key string, effect RepeatEffect, through int64, trailing bool) (streak int, last, previous *models.SessionEntry) {
	digests := map[string]string{} // key -> last executed digest (R4)
	resetSince := false
	for _, entry := range l.entries {
		if through > 0 && entry.Sequence > through {
			break
		}
		if entry.Type == models.SessionEntryUser {
			resetSince = true // R3
			continue
		}
		if entry.Tool == nil {
			continue
		}
		entryKey := entry.Tool.ArgumentsDigest
		executed := repeatExecuted(entry)
		entryEffect := repeatEntryEffect(entry)
		changed := false
		if executed && (entryEffect == RepeatEffectRead || entryEffect == RepeatEffectStateful) {
			digest := repeatEntryResultDigest(entry)
			prior, seen := digests[entryKey]
			changed = !seen || prior != digest
			digests[entryKey] = digest
		}
		if entryKey == key {
			if !repeatCounted(entry) {
				continue
			}
			if streak > 0 && resetSince {
				streak = 0
			}
			resetSince = false
			switch {
			case executed:
				if streak > 0 && last != nil && repeatEntryResultDigest(entry) == repeatEntryResultDigest(last) {
					streak++
				} else {
					streak = 1
				}
				previous, last = last, entry
			case repeatRefused(entry) && streak > 0:
				streak++
			}
			continue
		}
		switch {
		case executed && entry.State == models.ToolEntryCompleted &&
			(entryEffect == RepeatEffectWrite || entryEffect == RepeatEffectStateful):
			resetSince = true // R1: a successful write elsewhere
		case entryEffect == RepeatEffectControl && entry.State == models.ToolEntryCompleted && entry.Tool.Parked:
			resetSince = true // R2: a wait elapsed, a card answered, a child finished
		case effect == RepeatEffectStateful && changed:
			resetSince = true // R4: an observed change
		}
	}
	if trailing && streak > 0 && resetSince {
		streak = 0
	}
	return streak, last, previous
}

// evaluate decides one call before dispatch. stepKeys holds the keys of the
// calls of the same assistant entry already evaluated (key -> entry).
func (l *repeatLedger) evaluate(subject RepeatSubject, key string, stepKeys map[string]*models.SessionEntry) repeatVerdict {
	if subject.Effect == RepeatEffectControl || key == "" {
		return repeatVerdict{Kind: repeatRun}
	}
	if prior, ok := stepKeys[key]; ok {
		return repeatVerdict{Kind: repeatRefuse, Code: ToolGuardSameStepDuplicate, Prior: prior}
	}
	if subject.Effect == RepeatEffectWrite {
		// The duplicate rule ignores reset points: a done write stays done
		// for the whole Run.
		var succeeded *models.SessionEntry
		suppressed := 0
		for _, entry := range l.entries {
			if entry.Tool == nil || entry.Tool.ArgumentsDigest != key || !repeatCounted(entry) {
				continue
			}
			switch {
			case repeatExecuted(entry) && entry.State == models.ToolEntryCompleted:
				succeeded, suppressed = entry, 0
			case succeeded != nil && repeatGuardKind(entry) == action.RefusedLoopCallDuplicateSuppressed:
				suppressed++
			}
		}
		if succeeded != nil {
			count := 1 + suppressed + 1
			if count >= repeatLimitCount {
				return repeatVerdict{Kind: repeatFailRun, Code: action.RefusedLoopCallRepeatLimit, Prior: succeeded, Count: count}
			}
			return repeatVerdict{Kind: repeatSuppressWrite, Code: action.RefusedLoopCallDuplicateSuppressed, Prior: succeeded, Count: count}
		}
		// Never succeeded in this Run: a failed write may be retried under
		// the streak rule.
	}
	streak, last, _ := l.repeatStreak(key, subject.Effect, 0, true)
	count := streak + 1
	switch {
	case count <= 2:
		return repeatVerdict{Kind: repeatRun}
	case count < repeatLimitCount:
		return repeatVerdict{Kind: repeatRefuse, Code: action.RefusedLoopCallRepeat, Prior: last, Count: count}
	default:
		return repeatVerdict{Kind: repeatFailRun, Code: action.RefusedLoopCallRepeatLimit, Prior: last, Count: count}
	}
}

// noteAfter is called after an executed call's terminal transition (and
// observe). It returns the D9 note when this result equals the previous
// counted identical result with no reset between. waitOffered says the
// step's tools include wait; the note then suggests it for reads and
// stateful calls.
func (l *repeatLedger) noteAfter(entry *models.SessionEntry, waitOffered bool) (string, bool) {
	if !repeatExecuted(entry) {
		return "", false
	}
	effect := repeatEntryEffect(entry)
	if effect == RepeatEffectControl || effect == "" {
		return "", false
	}
	streak, last, previous := l.repeatStreak(entry.Tool.ArgumentsDigest, effect, entry.Sequence, false)
	if streak != 2 || last == nil || last.ID != entry.ID || previous == nil {
		return "", false
	}
	hint := waitOffered && (effect == RepeatEffectRead || effect == RepeatEffectStateful)
	return repeatNoteText(previous.Step, hint), true
}

// repeatStepCall is one call of a step, in model order, as the executor
// appended it pending.
type repeatStepCall struct {
	Entry   *models.SessionEntry
	Subject RepeatSubject
	Key     string // = Entry.Tool.ArgumentsDigest
}

// planStep evaluates every call of a step in model order before any is
// dispatched. When any verdict fails the Run, every other call that is not
// already refused or suppressed becomes run_stopping: nothing of the step
// runs. failed reports that the Run fails with repeat_limit after the step.
func (l *repeatLedger) planStep(calls []repeatStepCall) (verdicts []repeatVerdict, failed bool) {
	verdicts = make([]repeatVerdict, len(calls))
	stepKeys := map[string]*models.SessionEntry{}
	for i, call := range calls {
		verdicts[i] = l.evaluate(call.Subject, call.Key, stepKeys)
		if verdicts[i].Kind == repeatFailRun {
			failed = true
		}
		if call.Subject.Effect != RepeatEffectControl && call.Key != "" {
			if _, seen := stepKeys[call.Key]; !seen {
				stepKeys[call.Key] = call.Entry
			}
		}
	}
	if failed {
		for i := range verdicts {
			if verdicts[i].Kind == repeatRun {
				verdicts[i] = repeatVerdict{Kind: repeatRefuse, Code: ToolGuardRunStopping}
			}
		}
	}
	return verdicts, failed
}

// repeatRefusal is what the executor writes for a verdict that is not run:
// the pending -> error transition's Result, Guard and DuplicateOfEntryID,
// and for governed calls the refused action_calls row fields.
type repeatRefusal struct {
	Result             *models.ToolResult
	Guard              *models.ToolGuardOutcome
	DuplicateOfEntryID string
	// Message is the refusal text without appended result blocks
	// (RefusedLoopCallRequest.Message).
	Message string
	// ActionCallCode is the RefusedLoopCallRequest.Code; "" when the
	// refusal gets no action_calls row (same_step_duplicate, run_stopping).
	ActionCallCode string
	// DuplicateOfActionCallID is the prior counted entry's ActionCallID.
	DuplicateOfActionCallID string
	PriorStep               int64
	Count                   int
}

// refusal builds the refused result of a verdict; false for repeatRun.
func (v repeatVerdict) refusal() (*repeatRefusal, bool) {
	if v.Kind == repeatRun {
		return nil, false
	}
	out := &repeatRefusal{Count: v.Count}
	status := models.ToolResultRefused
	var extra []models.ContentPart
	switch v.Code {
	case action.RefusedLoopCallRepeat:
		out.Message = repeatRefusedText(v.Count, repeatPriorStep(v.Prior))
	case action.RefusedLoopCallRepeatLimit:
		out.Message = repeatLimitText(v.Count)
	case action.RefusedLoopCallDuplicateSuppressed:
		status = models.ToolResultDuplicateSuppressed
		out.Message = repeatSuppressedText(repeatPriorStep(v.Prior))
		if v.Prior != nil && v.Prior.Tool != nil && v.Prior.Tool.Result != nil {
			extra = append(extra, v.Prior.Tool.Result.Content...)
		}
	case ToolGuardSameStepDuplicate:
		prior := ""
		if v.Prior != nil {
			prior = v.Prior.ToolCallID
		}
		out.Message = repeatSameStepText(prior)
	case ToolGuardRunStopping:
		out.Message = repeatRunStoppingText
	default:
		return nil, false
	}
	switch v.Code {
	case action.RefusedLoopCallRepeat, action.RefusedLoopCallRepeatLimit, action.RefusedLoopCallDuplicateSuppressed:
		out.ActionCallCode = v.Code
		if v.Prior != nil {
			out.DuplicateOfEntryID = v.Prior.ID
			out.DuplicateOfActionCallID = v.Prior.ActionCallID
			out.PriorStep = v.Prior.Step
		}
	case ToolGuardSameStepDuplicate:
		if v.Prior != nil {
			out.DuplicateOfEntryID = v.Prior.ID
		}
	}
	out.Guard = &models.ToolGuardOutcome{Kind: v.Code, Count: v.Count, PriorEntryID: out.DuplicateOfEntryID}
	content := append([]models.ContentPart{{Type: models.ContentPartText, Text: out.Message}}, extra...)
	out.Result = &models.ToolResult{Status: status, IsError: true, Content: content}
	return out, true
}

func repeatPriorStep(entry *models.SessionEntry) int64 {
	if entry == nil {
		return 0
	}
	return entry.Step
}

// repeatLimitRunFailure is the Run failure of a step with a failRun verdict;
// the executor returns it once every tool entry of the step is terminal and
// the buffered notes are appended.
func repeatLimitRunFailure() modelstep.GuardRunFailure {
	failure, _ := modelstep.GuardRunFailureFor(action.RunFailureCodeRepeatLimit)
	return failure
}
