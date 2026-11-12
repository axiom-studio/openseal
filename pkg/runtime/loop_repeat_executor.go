package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The executor side of D9 (section 08 §5.5): the LoopToolExecutor stamps every
// tool entry of a step with its repeat fields before appending it pending,
// opens one loopRepeatGuard per step, dispatches only the calls the guard
// lets run, and finishes the guard once the step's entries are terminal.
//
//	stampRepeatFields(entry.Tool, subject)           // PlanToolEntries, before the append
//	guard, err := startLoopRepeatGuard(ctx, cfg, calls) // calls in model order
//	refused, err := guard.ApplyRefusals(ctx)         // before dispatching any call
//	for each call with guard.Dispatch(entryID):
//	    req = withRepeatKernelFields(req)            // on every transition of an executed call
//	    guard.Observe(transitioned entry)
//	failure, err := guard.Finish(ctx)                // once every entry of the step is terminal
//
// The guard reads only the Run's session entries, so the same calls give the
// same verdicts after a claim, a park or a crash (D14). ResumeOpenTools opens
// a guard per step for the entries it resumes; entries that are no longer
// pending are not evaluated again.

// RepeatNoteSourceKey is the logical and dedupe key of the D9 note written
// after tool entry toolEntryID.
func RepeatNoteSourceKey(toolEntryID string) string { return "repeat-note:" + toolEntryID }

// maxRepeatNotesPerAppend keeps one note append inside the store's batch cap.
const maxRepeatNotesPerAppend = 128

// stampRepeatFields sets a tool entry's ArgumentsDigest and RepeatEffect from
// the call's repeat subject (§5.5 step 1).
func stampRepeatFields(tool *ToolEntry, subject RepeatSubject) error {
	if tool == nil {
		return errors.New("repeat fields need a tool entry")
	}
	if !subject.Effect.Valid() {
		return fmt.Errorf("repeat effect %q is invalid", subject.Effect)
	}
	key, err := repeatKey(subject)
	if err != nil {
		return err
	}
	tool.ArgumentsDigest = key
	tool.RepeatEffect = string(subject.Effect)
	return nil
}

// withRepeatKernelFields adds the D9 kernel fields to a transition of an
// executed call: ResultDigest on a terminal transition that carries a result
// (§5.5 step 4), and Parked when the entry leaves an awaiting_* state, which
// makes a completed control entry a reset point (R2). A transition that
// already carries a Guard is a refusal, not an execution, and gets no digest.
func withRepeatKernelFields(req ToolEntryTransitionRequest) ToolEntryTransitionRequest {
	var patch ToolEntryKernelPatch
	if req.Kernel != nil {
		patch = *req.Kernel
	}
	changed := false
	if req.To.Terminal() && req.Result != nil && patch.Guard == nil && patch.ResultDigest == "" {
		patch.ResultDigest = modelstep.ToolResultDigest(req.Result)
		changed = true
	}
	if req.From.Awaiting() && patch.Parked == nil {
		parked := true
		patch.Parked = &parked
		changed = true
	}
	if changed {
		req.Kernel = &patch
	}
	return req
}

// loopRepeatCall is one tool entry of a step, as appended, in model order.
// The key and effect are read from the entry (stampRepeatFields).
type loopRepeatCall struct {
	Entry *SessionEntry
	// Governed is set for a Skill/MCP action call: a refusal by count then
	// also gets its refused action_calls row (§5.7). Nil for host and kernel
	// tools.
	Governed *loopRepeatGovernedCall
}

// loopRepeatGovernedCall is what the refused action_calls row of a governed
// call needs; Arguments are the executed (inner) action arguments.
type loopRepeatGovernedCall struct {
	DeploymentID    string
	SkillID         string
	SkillVersion    string
	Action          string
	BindingID       string
	BindingRevision int64
	Risk            skill.RiskLevel
	SideEffect      skill.SideEffect
	InputSchema     map[string]interface{}
	Arguments       map[string]interface{}
}

// loopRepeatSessionStore is the part of AgentSessionStore the guard uses.
type loopRepeatSessionStore interface {
	conversation.LoopSessionEntryLister
	AppendAgentSessionEntries(ctx context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error)
	TransitionToolEntry(ctx context.Context, req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error)
}

// refusedLoopCallRecorder stores a refused governed loop call
// (*ActionCoordinator).
type refusedLoopCallRecorder interface {
	RecordRefusedLoopCall(ctx context.Context, req action.RefusedLoopCallRequest) (*ActionProposalResult, error)
}

type loopRepeatGuardConfig struct {
	Sessions loopRepeatSessionStore
	// Refusals records the refused row of a governed refusal; required only
	// when a governed call is refused by count.
	Refusals refusedLoopCallRecorder
	Run      *AgentRun // the Run whose lease the executor holds; SessionID is its session
	WorkerID string    // the Run lease owner
	// WaitOffered says the step's tools include wait; the note then adds
	// the wait hint for reads and stateful calls.
	WaitOffered bool
	Now         func() time.Time
}

// loopRepeatGuard is the D9 state of one step's tool entries.
type loopRepeatGuard struct {
	cfg      loopRepeatGuardConfig
	stepID   string
	calls    []loopRepeatCall
	index    map[string]int // entry id -> calls index
	verdicts []repeatVerdict
	pending  []bool // the entry was pending when the guard started: it was evaluated
	failed   bool

	mu     sync.Mutex
	ledger *repeatLedger
}

// startLoopRepeatGuard loads the Run's ledger and evaluates every pending
// call of the step in model order before any is dispatched (§5.5 step 2).
// When a verdict fails the Run (or an entry of the step already did, before
// a crash), every pending call that would run becomes run_stopping.
func startLoopRepeatGuard(ctx context.Context, cfg loopRepeatGuardConfig, calls []loopRepeatCall) (*loopRepeatGuard, error) {
	if cfg.Sessions == nil || cfg.Run == nil || cfg.Run.SessionID == "" || cfg.WorkerID == "" {
		return nil, errors.New("repeat guard needs a session store, the leased Run and its session")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	g := &loopRepeatGuard{
		cfg: cfg, calls: calls, index: make(map[string]int, len(calls)),
		verdicts: make([]repeatVerdict, len(calls)), pending: make([]bool, len(calls)),
	}
	for i, call := range calls {
		entry := call.Entry
		if entry == nil || entry.Type != SessionEntryTool || entry.Tool == nil || entry.ID == "" || entry.RunID != cfg.Run.ID {
			return nil, fmt.Errorf("repeat guard call %d is not a tool entry of Run %s", i, cfg.Run.ID)
		}
		if g.stepID == "" {
			g.stepID = entry.StepID
		}
		if entry.StepID != g.stepID {
			return nil, errors.New("repeat guard calls belong to more than one step")
		}
		if _, dup := g.index[entry.ID]; dup {
			return nil, fmt.Errorf("repeat guard got tool entry %s twice", entry.ID)
		}
		g.index[entry.ID] = i
		if entry.State == ToolEntryPending {
			effect := RepeatEffect(entry.Tool.RepeatEffect)
			if !effect.Valid() || (effect != RepeatEffectControl && entry.Tool.ArgumentsDigest == "") {
				return nil, fmt.Errorf("tool entry %s has no repeat key or effect", entry.ID)
			}
		}
	}
	ledger, err := loadRepeatLedger(ctx, cfg.Sessions, cfg.Run.Scope, cfg.Run.SessionID, cfg.Run.ID)
	if err != nil {
		return nil, err
	}
	g.ledger = ledger
	stepKeys := map[string]*SessionEntry{}
	for i, call := range calls {
		entry := call.Entry
		effect := RepeatEffect(entry.Tool.RepeatEffect)
		key := entry.Tool.ArgumentsDigest
		switch {
		case entry.State == ToolEntryPending:
			g.pending[i] = true
			g.verdicts[i] = ledger.evaluate(RepeatSubject{Effect: effect}, key, stepKeys)
			if g.verdicts[i].Kind == repeatFailRun {
				g.failed = true
			}
		case repeatGuardKind(entry) == action.RefusedLoopCallRepeatLimit:
			g.failed = true // decided before a crash or park
		}
		if effect != RepeatEffectControl && key != "" {
			if _, seen := stepKeys[key]; !seen {
				stepKeys[key] = entry
			}
		}
	}
	if g.failed {
		for i := range g.verdicts {
			if g.pending[i] && g.verdicts[i].Kind == repeatRun {
				g.verdicts[i] = repeatVerdict{Kind: repeatRefuse, Code: ToolGuardRunStopping}
			}
		}
	}
	return g, nil
}

// Dispatch reports whether the executor may run entry entryID: it was pending
// when the guard started and its verdict is run.
func (g *loopRepeatGuard) Dispatch(entryID string) bool {
	i, ok := g.index[entryID]
	return ok && g.pending[i] && g.verdicts[i].Kind == repeatRun
}

// Verdict returns the guard's verdict for entry entryID; ok is false for an
// entry that was not pending when the guard started.
func (g *loopRepeatGuard) Verdict(entryID string) (repeatVerdict, bool) {
	i, ok := g.index[entryID]
	if !ok || !g.pending[i] {
		return repeatVerdict{}, false
	}
	return g.verdicts[i], true
}

// Failed reports that the step fails the Run with repeat_limit.
func (g *loopRepeatGuard) Failed() bool { return g.failed }

func (g *loopRepeatGuard) lease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: g.cfg.WorkerID, Now: g.cfg.Now()}
}

// ApplyRefusals moves every pending call whose verdict is not run to error
// with its refusal (§5.5 step 3): no policy, approval, dispatch or VM. A
// governed refusal by count first stores its refused action_calls row and
// names it on the transition. It returns the transitioned entries in model
// order. A lost lease or a concurrent close comes back as the store's error
// (ErrLeaseLost, ErrToolEntryStateConflict).
func (g *loopRepeatGuard) ApplyRefusals(ctx context.Context) ([]*SessionEntry, error) {
	var out []*SessionEntry
	for i, call := range g.calls {
		if !g.pending[i] {
			continue
		}
		refusal, ok := g.verdicts[i].refusal()
		if !ok {
			continue
		}
		entry := call.Entry
		actionCallID := ""
		if call.Governed != nil && refusal.ActionCallCode != "" && refusal.DuplicateOfActionCallID != "" {
			id, err := g.recordRefusedRow(ctx, entry, call.Governed, refusal)
			if err != nil {
				return out, err
			}
			actionCallID = id
		}
		next := cloneSessionEntry(entry)
		next.State = ToolEntryError
		next.ActionCallID = firstNonEmpty(entry.ActionCallID, actionCallID)
		next.Tool.Result = refusal.Result
		next.Tool.Guard = refusal.Guard
		next.Tool.DuplicateOfEntryID = refusal.DuplicateOfEntryID
		lease := g.lease()
		result, err := g.cfg.Sessions.TransitionToolEntry(ctx, ToolEntryTransitionRequest{
			Scope: g.cfg.Run.Scope, EntryID: entry.ID, RunID: g.cfg.Run.ID, Lease: lease,
			From: ToolEntryPending, ExpectedRevision: entry.Revision, To: ToolEntryError,
			ActionCallID: actionCallID, Result: refusal.Result, Tokens: modelstep.EstimateEntryTokens(next),
			Kernel: &ToolEntryKernelPatch{Guard: refusal.Guard, DuplicateOfEntryID: refusal.DuplicateOfEntryID},
			Now:    lease.Now,
		})
		if err != nil {
			return out, fmt.Errorf("refuse repeated tool call %s: %w", entry.ToolCallID, err)
		}
		g.Observe(result.Entry)
		g.calls[i].Entry = result.Entry
		out = append(out, result.Entry)
	}
	return out, nil
}

func (g *loopRepeatGuard) recordRefusedRow(ctx context.Context, entry *SessionEntry, governed *loopRepeatGovernedCall, refusal *repeatRefusal) (string, error) {
	if g.cfg.Refusals == nil {
		return "", errors.New("a governed repeat refusal needs the action coordinator")
	}
	lease := g.lease()
	result, err := g.cfg.Refusals.RecordRefusedLoopCall(ctx, action.RefusedLoopCallRequest{
		Scope: g.cfg.Run.Scope, Run: g.cfg.Run, StepID: entry.StepID, DeploymentID: governed.DeploymentID,
		SkillID: governed.SkillID, SkillVersion: governed.SkillVersion, Action: governed.Action,
		BindingID: governed.BindingID, BindingRevision: governed.BindingRevision,
		Risk: governed.Risk, SideEffect: governed.SideEffect, InputSchema: governed.InputSchema, Arguments: governed.Arguments,
		Code: refusal.ActionCallCode, Message: refusal.Message, DuplicateOfActionCallID: refusal.DuplicateOfActionCallID,
		PriorStep: refusal.PriorStep, Count: refusal.Count, Lease: &lease,
	})
	if err != nil {
		return "", fmt.Errorf("record refused action call for %s: %w", entry.ToolCallID, err)
	}
	if result == nil || result.Call == nil || result.Call.ID == "" {
		return "", errors.New("record refused action call returned no row")
	}
	return result.Call.ID, nil
}

// Observe records an entry the executor transitioned (or appended) during
// the step. Safe for concurrent dispatch.
func (g *loopRepeatGuard) Observe(entry *SessionEntry) {
	if entry == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ledger.observe(entry)
	if i, ok := g.index[entry.ID]; ok {
		g.calls[i].Entry = entry
	}
}

// Finish ends the step's D9 work (§5.5 step 5). It does nothing while an
// entry of the step is still open (the step parked): notes must never sit
// between tool results, and the guard of a later resume finishes the step.
// Once every entry is terminal it appends the D9 notes of the step's executed
// calls after them (deduped by RepeatNoteSourceKey, so a resumed or repeated
// finish writes each note once) and returns the step's Run failure, if any:
// repeat_limit when a call was repeated five times, else error_streak when
// the Run's last loopErrorStreakLimit steps all ended in errors (G13).
func (g *loopRepeatGuard) Finish(ctx context.Context) (*agentrun.LoopFailure, error) {
	g.mu.Lock()
	var stepEntries []*SessionEntry
	for _, entry := range g.ledger.entries {
		if entry.Type == SessionEntryTool && entry.StepID == g.stepID {
			stepEntries = append(stepEntries, entry)
		}
	}
	open := false
	for _, call := range g.calls {
		if !call.Entry.State.Terminal() {
			open = true
		}
	}
	for _, entry := range stepEntries {
		if !entry.State.Terminal() {
			open = true
		}
	}
	var notes []*SessionEntry
	if !open {
		for _, entry := range stepEntries {
			text, ok := g.ledger.noteAfter(entry, g.cfg.WaitOffered)
			if !ok {
				continue
			}
			notes = append(notes, g.repeatNote(entry, text))
		}
	}
	streak := 0
	if !open && !g.failed {
		streak = loopErrorStreak(g.ledger.entries)
	}
	g.mu.Unlock()
	if open {
		return nil, nil
	}
	for start := 0; start < len(notes); start += maxRepeatNotesPerAppend {
		end := min(start+maxRepeatNotesPerAppend, len(notes))
		lease := g.lease()
		if _, err := g.cfg.Sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
			Scope: g.cfg.Run.Scope, SessionID: g.cfg.Run.SessionID, RunID: g.cfg.Run.ID,
			Lease: &lease, Entries: notes[start:end], Now: lease.Now,
		}); err != nil {
			return nil, fmt.Errorf("append repeat notes: %w", err)
		}
	}
	if g.failed {
		return loopFailureFromGuard(repeatLimitRunFailure()), nil
	}
	if streak >= loopErrorStreakLimit {
		failure, _ := modelstep.GuardRunFailureFor(modelstep.RunFailureCodeErrorStreak)
		return loopFailureFromGuard(failure), nil
	}
	return nil, nil
}

func (g *loopRepeatGuard) repeatNote(after *SessionEntry, text string) *SessionEntry {
	key := RepeatNoteSourceKey(after.ID)
	data, _ := json.Marshal(map[string]string{"toolEntryId": after.ID, "toolCallId": after.ToolCallID})
	note := &SessionEntry{
		ID: SessionEntryID(g.cfg.Run.SessionID, key), Scope: g.cfg.Run.Scope, SessionID: g.cfg.Run.SessionID,
		RunID: g.cfg.Run.ID, StepID: after.StepID, Step: after.Step, Type: SessionEntryNote, SourceKey: key,
		Note: &NoteEntry{Kind: NoteRepeat, Text: text, Data: data},
	}
	note.Tokens = modelstep.EstimateEntryTokens(note)
	return note
}

func loopFailureFromGuard(failure modelstep.GuardRunFailure) *agentrun.LoopFailure {
	return &agentrun.LoopFailure{Kind: failure.Kind, Code: failure.Code, Message: failure.Message, Explainable: failure.Explainable}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
