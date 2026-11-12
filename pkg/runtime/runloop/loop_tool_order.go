package runloop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Execution order inside a step (SPEC §5.2) and the four LoopToolExecutor
// phases built on it.
//
//   - Calls run in model order. A maximal run of consecutive none/read calls
//     runs concurrently, at most loopToolMaxConcurrent at a time; a
//     write/stateful call is a barrier: it waits for the calls before it and
//     blocks the calls after it.
//   - Once a call reaches awaiting_approval/awaiting_event (or waits for a
//     foreign call lease), later reads may still run, but the first later
//     non-read call stays pending and is a standing barrier: it and every
//     call after it (reads included) wait; ResumeOpenTools runs them in order
//     after the earlier call settled.
//   - Settle-phase control calls run only in SettleStep (phase 10).

// loopToolMaxConcurrent bounds the concurrent calls of one step (host reads
// and governed reads share it).
const loopToolMaxConcurrent = models.GovernedMaxConcurrentReads

// dispatchInOrder runs entries indexes (in model order) with run, applying
// the ordering rules. A non-read call reached while an earlier call is held
// stops the dispatch: it and every later call stay as they are.
func (r *stepToolRun) dispatchInOrder(ctx context.Context, indexes []int, run func(context.Context, int) error) error {
	for k := 0; k < len(indexes); {
		if r.stopped() {
			return nil
		}
		i := indexes[k]
		if r.tools[i].concurrent() {
			var batch []int
			for k < len(indexes) && r.tools[indexes[k]].concurrent() {
				batch = append(batch, indexes[k])
				k++
			}
			if err := r.runConcurrent(ctx, batch, run); err != nil {
				return err
			}
			continue
		}
		k++
		if r.isHeld() {
			// A held call before it: this non-read call waits for its decision
			// and blocks every later call, reads included.
			return nil
		}
		if err := run(ctx, i); err != nil {
			return err
		}
	}
	return nil
}

// runConcurrent runs a batch of none/read calls, at most
// loopToolMaxConcurrent at once. The Run writes they make are adopted
// together (loopRunChain.adoptBatch).
func (r *stepToolRun) runConcurrent(ctx context.Context, batch []int, run func(context.Context, int) error) error {
	if len(batch) == 1 {
		return run(ctx, batch[0])
	}
	r.mu.Lock()
	r.batch, r.batchRuns = true, nil
	r.mu.Unlock()
	var wg sync.WaitGroup
	slots := make(chan struct{}, loopToolMaxConcurrent)
	errs := make([]error, len(batch))
	for n, i := range batch {
		wg.Add(1)
		slots <- struct{}{}
		go func(n, i int) {
			defer wg.Done()
			defer func() { <-slots }()
			errs[n] = run(ctx, i)
		}(n, i)
	}
	wg.Wait()
	r.mu.Lock()
	r.batch = false
	r.chain.adoptBatch(r.batchRuns)
	r.batchRuns = nil
	r.mu.Unlock()
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Phase 8: the inline phase

// ExecuteStepTools runs the inline phase on the pending entries the loop
// appended for this step (SPEC §3.1 phase 8). A settle-phase call stays
// pending and SettlePending asks the loop to call SettleStep after
// FinishTurn+applyStep; a planned park (approval, card, event, maintenance)
// skips the settle phase.
func (x *ClaimToolExecutor) ExecuteStepTools(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	if len(in.Entries) == 0 {
		return agentrun.StepToolsOutcome{}, nil
	}
	r, err := x.newStepToolRun(in, in.Step, in.Entries)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	if err := r.precheck(ctx); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	if err := r.startGuard(ctx); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	var indexes []int
	for i := range r.entries {
		if r.dispatchable(i) {
			indexes = append(indexes, i)
		}
	}
	if err := r.dispatchInOrder(ctx, indexes, r.runCall); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	return r.endOfToolWork(ctx)
}

// endOfToolWork turns the step's state into its outcome: failure and budget
// exits first, then the step-end park, then the settle-phase call, then the
// guard's Run failure once every entry is terminal.
func (r *stepToolRun) endOfToolWork(ctx context.Context) (agentrun.StepToolsOutcome, error) {
	out := r.outcome()
	if out.Failure != nil || out.BudgetExhausted != nil || out.Final != nil || r.ended {
		return out, nil
	}
	park, settled, err := r.planPark(ctx)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	out = r.outcome()
	if park != nil {
		out.Park = park
		return out, nil
	}
	if settled {
		return out, nil
	}
	if settle := r.openSettleEntry(); settle != nil {
		if r.onlyOpen(settle) {
			out.SettlePending = true
		}
		return out, nil
	}
	failure, err := r.finishGuard(ctx)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	out = r.outcome()
	if failure != nil {
		out.Failure = failure
	}
	return out, nil
}

// openSettleEntry is the step's open settle-phase control entry, if any.
func (r *stepToolRun) openSettleEntry() *models.SessionEntry {
	for i := range r.entries {
		if entry := r.entry(i); r.tools[i].settle() && entry.State.Open() {
			return entry
		}
	}
	return nil
}

// refuseUnsettled closes the pending/running settle-phase entry of a step
// that was canceled or failed before its settle phase, as not run. The
// model sees the refusal in its next step.
func (r *stepToolRun) refuseUnsettled(ctx context.Context, settle *models.SessionEntry, status models.AgentTurnStatus) error {
	if settle.State != models.ToolEntryPending && settle.State != models.ToolEntryRunning || !r.onlyOpen(settle) {
		return nil
	}
	for i := range r.entries {
		if r.entry(i).ID == settle.ID {
			return r.refuse(ctx, i, control.ErrorResult(models.ToolResultRefused, loopToolNotRunCode,
				fmt.Sprintf("not run: the step that made this call was %s before it ran", status), nil), nil)
		}
	}
	return nil
}

// onlyOpen reports that entry is the step's only open entry.
func (r *stepToolRun) onlyOpen(entry *models.SessionEntry) bool {
	for i := range r.entries {
		if other := r.entry(i); other.ID != entry.ID && other.State.Open() {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Phase 10: the settle phase

// SettleStep settles the step's single settle-phase control call after
// FinishTurn+applyStep (SPEC §3.1 phase 10). in.Step is the finished, applied
// step and in.Run the Run after applying it.
func (x *ClaimToolExecutor) SettleStep(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	if in.Step == nil {
		return agentrun.StepToolsOutcome{}, errors.New("settle needs the finished step")
	}
	entries, err := x.stepEntries(ctx, in.Run, x.sessionOf(in), in.Step.ID)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	r, err := x.newStepToolRun(in, in.Step, entries)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	if err := r.settle(ctx); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	return r.endOfSettle(ctx)
}

// settle runs the step's open settle-phase control entry when every other
// entry of the step is terminal. When an earlier call of the step was denied,
// superseded or expired, the entry is not run.
func (r *stepToolRun) settle(ctx context.Context) error {
	for i := range r.entries {
		entry := r.entry(i)
		if !r.tools[i].settle() || !entry.State.Open() {
			continue
		}
		if !r.onlyOpen(entry) {
			return nil
		}
		switch entry.State {
		case models.ToolEntryAwaitingChild, models.ToolEntryAwaitingEvent:
			return r.resumeControl(ctx, i)
		}
		if status := r.earlierBlockingStatus(i); status != "" {
			return r.refuse(ctx, i, control.ErrorResult(models.ToolResultRefused, loopToolNotRunCode,
				fmt.Sprintf("not run: an earlier call in this step was %s", status), nil), nil)
		}
		if r.x.cfg.Control == nil {
			return r.refuse(ctx, i, control.ErrorResult(models.ToolResultRefused, loopToolNotAvailableCode,
				"This tool is not available in this conversation.", nil), nil)
		}
		result, err := r.x.cfg.Control.Settle(ctx, r.controlContext(i))
		if err != nil {
			return err
		}
		return r.applyControl(ctx, i, result)
	}
	return nil
}

// earlierBlockingStatus is the result status of an earlier call of the step
// that ended without its decision being carried out ("" when none).
func (r *stepToolRun) earlierBlockingStatus(i int) models.ToolResultStatus {
	for j := 0; j < i; j++ {
		entry := r.entry(j)
		if entry.Tool.Result == nil {
			continue
		}
		switch status := entry.Tool.Result.Status; status {
		case models.ToolResultDenied, models.ToolResultChangesRequested, models.ToolResultSuperseded, models.ToolResultExpired:
			return status
		}
	}
	return ""
}

func (r *stepToolRun) resumeControl(ctx context.Context, i int) error {
	if r.x.cfg.Control == nil {
		return fmt.Errorf("control tool entry %s is open but there is no control executor", r.entry(i).ID)
	}
	resumed, err := r.x.cfg.Control.Resume(ctx, r.controlContext(i))
	if err != nil {
		return err
	}
	if !resumed.Resolved {
		park := *resumed.Park
		r.mu.Lock()
		r.park, r.held = &park, true
		r.mu.Unlock()
		return nil
	}
	return r.applyControl(ctx, i, resumed.Result)
}

func (r *stepToolRun) endOfSettle(ctx context.Context) (agentrun.StepToolsOutcome, error) {
	r.mu.Lock()
	park := r.park
	r.mu.Unlock()
	out := r.outcome()
	if park != nil {
		out.Park = park
		return out, nil
	}
	if out.Failure != nil || out.Final != nil || r.ended {
		return out, nil
	}
	if err := r.startGuard(ctx); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	failure, err := r.finishGuard(ctx)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	out = r.outcome()
	out.Failure = failure
	return out, nil
}

// ---------------------------------------------------------------------------
// Phase 2: resume of open entries of earlier steps

// ResumeOpenTools settles the open tool entries this Run left in earlier
// steps (SPEC §3.1 phase 2, §3.7), step by step in step order and inside a
// step in model order:
//
//   - governed entries: GovernedToolExecutor.Resume (pending entries that
//     never got an action call are proposed like new calls);
//   - host entries: pending runs; running is the D14 rule (reads run again,
//     writes become interrupted);
//   - capability and inline control entries: run again (pure or idempotent);
//   - control entries awaiting a child or an event: ControlExecutor.Resume;
//   - a pending/running settle-phase control entry: Settle again with its own
//     (finished, applied) step once the step's other entries are terminal,
//     unless an earlier call was denied, superseded or expired.
//
// A step that still waits stops the resume: later steps are not touched.
func (x *ClaimToolExecutor) ResumeOpenTools(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	if in.Run == nil {
		return agentrun.StepToolsOutcome{}, errors.New("resume needs the Run")
	}
	sessionID := x.sessionOf(in)
	if sessionID == "" {
		return agentrun.StepToolsOutcome{}, nil
	}
	open, err := x.openEntries(ctx, in.Run, sessionID)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	var steps []string
	seen := map[string]bool{}
	for _, entry := range open {
		if !seen[entry.StepID] {
			seen[entry.StepID] = true
			steps = append(steps, entry.StepID)
		}
	}
	total := agentrun.StepToolsOutcome{}
	run := in.Run
	for _, stepID := range steps {
		stepIn := in
		stepIn.Run = run
		out, err := x.resumeStep(ctx, stepIn, sessionID, stepID)
		if err != nil {
			return agentrun.StepToolsOutcome{}, err
		}
		total.HostToolCalls += out.HostToolCalls
		if out.Run != nil {
			run, total.Run = out.Run, out.Run
		}
		if out.Park != nil || out.Final != nil || out.Failure != nil || out.BudgetExhausted != nil ||
			models.IsTerminalAgentRunStatus(run.Status) || out.SettlePending {
			total.Park, total.Final, total.Failure, total.BudgetExhausted = out.Park, out.Final, out.Failure, out.BudgetExhausted
			total.SettlePending = out.SettlePending
			return total, nil
		}
		if x.stepStillOpen(ctx, run, sessionID, stepID) {
			// A wait target was decided meanwhile: the next step resumes it.
			return total, nil
		}
	}
	return total, nil
}

func (x *ClaimToolExecutor) resumeStep(ctx context.Context, in agentrun.StepToolsInput, sessionID, stepID string) (agentrun.StepToolsOutcome, error) {
	step, err := x.cfg.Turns.GetAgentTurn(ctx, in.Run.Scope, stepID)
	if err != nil {
		return agentrun.StepToolsOutcome{}, fmt.Errorf("load step %s of open tool entries: %w", stepID, err)
	}
	entries, err := x.stepEntries(ctx, in.Run, sessionID, stepID)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	assistant, err := x.cfg.Sessions.GetSessionEntry(ctx, in.Run.Scope, models.SessionEntryID(sessionID, models.AssistantEntryKey(stepID)))
	switch {
	case err == nil && assistant != nil && assistant.Type == models.SessionEntryAssistant:
		in.Assistant = assistant
	case err == nil, errors.Is(err, models.ErrSessionEntryNotFound):
		in.Assistant = nil
	default:
		return agentrun.StepToolsOutcome{}, err
	}
	r, err := x.newStepToolRun(in, step, entries)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	// Pending entries may never have gone through phase 8 (a crash or lease
	// loss between the append and ExecuteStepTools): run its pre-dispatch
	// checks. Entries it already refused are terminal; the rest pass again.
	if err := r.precheck(ctx); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	if err := r.startGuard(ctx); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	var indexes []int
	for i := range r.entries {
		entry := r.entry(i)
		if !entry.State.Open() || r.tools[i].settle() && (entry.State == models.ToolEntryPending || entry.State == models.ToolEntryRunning) {
			continue
		}
		if entry.State == models.ToolEntryPending && r.guard != nil && !r.guard.Dispatch(entry.ID) {
			continue
		}
		indexes = append(indexes, i)
	}
	if err := r.dispatchInOrder(ctx, indexes, r.resumeCall); err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	out := r.outcome()
	if out.Failure != nil || out.BudgetExhausted != nil || out.Final != nil || r.ended {
		return out, nil
	}
	park, settled, err := r.planPark(ctx)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	if park != nil || settled {
		out = r.outcome()
		out.Park = park
		return out, nil
	}
	if settle := r.openSettleEntry(); settle != nil {
		switch {
		case in.Step != nil && step.ID == in.Step.ID && step.Status == models.AgentTurnStatusRunning:
			// The entry's step is the claim's resumed running step (a crash
			// before its FinishTurn): it settles after FinishTurn+applyStep,
			// like a step that just ran its inline phase.
			out = r.outcome()
			out.SettlePending = r.onlyOpen(settle)
			return out, nil
		case step.Status == models.AgentTurnStatusCanceled || step.Status == models.AgentTurnStatusFailed:
			// The step ended (duration budget, host outage) before its
			// settle phase: the call never ran and never will.
			if err := r.refuseUnsettled(ctx, settle, step.Status); err != nil {
				return agentrun.StepToolsOutcome{}, err
			}
			settle = nil
		}
	}
	if settle := r.openSettleEntry(); settle != nil {
		if err := r.settle(ctx); err != nil {
			return agentrun.StepToolsOutcome{}, err
		}
		r.mu.Lock()
		controlPark := r.park
		r.mu.Unlock()
		out = r.outcome()
		if controlPark != nil {
			out.Park = controlPark
			return out, nil
		}
		if out.Failure != nil || out.Final != nil || r.ended {
			return out, nil
		}
	}
	failure, err := r.finishGuard(ctx)
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	out = r.outcome()
	out.Failure = failure
	return out, nil
}

// resumeCall settles one open (not settle-phase pending) entry by route and
// state.
func (r *stepToolRun) resumeCall(ctx context.Context, i int) error {
	entry, tool := r.entry(i), r.tools[i]
	switch tool.Route {
	case loopRouteGoverned:
		if r.x.cfg.Governed == nil {
			return fmt.Errorf("governed tool entry %s is open but there is no governed executor", entry.ID)
		}
		if entry.State == models.ToolEntryPending && entry.ActionCallID == "" {
			return r.runGoverned(ctx, i)
		}
		return r.resumeGoverned(ctx, i)
	case loopRouteHost:
		if entry.State == models.ToolEntryRunning {
			state, result := modelstep.HostToolResumeState(tool.Host)
			if state != models.ToolEntryPending {
				return r.transition(ctx, i, state, result, nil, "", nil, false)
			}
			if err := r.transition(ctx, i, models.ToolEntryPending, nil, nil, "", nil, false); err != nil {
				return err
			}
		}
		return r.runHost(ctx, i)
	case loopRouteControl:
		switch entry.State {
		case models.ToolEntryAwaitingChild, models.ToolEntryAwaitingEvent:
			return r.resumeControl(ctx, i)
		case models.ToolEntryPending, models.ToolEntryRunning:
			return r.runControlInline(ctx, i)
		}
		return fmt.Errorf("control tool entry %s is %s", entry.ID, entry.State)
	case loopRouteCapability:
		return r.runCapability(ctx, i)
	}
	if entry.State == models.ToolEntryPending || entry.State == models.ToolEntryRunning {
		return r.runCall(ctx, i)
	}
	return fmt.Errorf("tool entry %s of an unknown tool is %s", entry.ID, entry.State)
}

// ---------------------------------------------------------------------------
// Re-plan after a failed park CAS (SPEC §3.2)

// ReplanPark re-plans the park after the loop's park CAS failed. in.Run is
// the reloaded Run. Held approvals created before a newer conversation message
// are superseded; a decided approval, a settled card or a resolved control
// wait gives nil (do not park).
func (x *ClaimToolExecutor) ReplanPark(ctx context.Context, in agentrun.StepToolsInput) (*agentrun.LoopPark, error) {
	if in.Run == nil {
		return nil, errors.New("replan needs the Run")
	}
	sessionID := x.sessionOf(in)
	if sessionID == "" {
		return nil, nil
	}
	open, err := x.openEntries(ctx, in.Run, sessionID)
	if err != nil {
		return nil, err
	}
	if len(open) == 0 {
		return nil, nil
	}
	r, err := x.newStepToolRun(in, in.Step, open)
	if err != nil {
		return nil, err
	}
	var governed []*models.SessionEntry
	for i, entry := range r.entries {
		if r.tools[i].Route == loopRouteGoverned {
			governed = append(governed, entry)
		}
	}
	if len(governed) > 0 && x.cfg.Governed != nil {
		plan, err := x.cfg.Governed.ReplanPark(ctx, in.Run, governed)
		if err != nil {
			return nil, err
		}
		if err := r.applyReplaced(ctx, plan); err != nil {
			return nil, err
		}
		if plan.Settled {
			return nil, nil
		}
		if plan.Park != nil {
			return &agentrun.LoopPark{Status: plan.Park.Status, Wake: plan.Park.Wake, Summary: plan.Park.Summary}, nil
		}
	}
	for i := range r.entries {
		entry := r.entry(i)
		if r.tools[i].Route != loopRouteControl || (entry.State != models.ToolEntryAwaitingChild && entry.State != models.ToolEntryAwaitingEvent) {
			continue
		}
		// The control wait is resolved against its own step.
		step, err := x.cfg.Turns.GetAgentTurn(ctx, in.Run.Scope, entry.StepID)
		if err != nil {
			return nil, err
		}
		r.step = step
		if err := r.resumeControl(ctx, i); err != nil {
			return nil, err
		}
		r.mu.Lock()
		park := r.park
		r.mu.Unlock()
		return park, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Session reads

func (x *ClaimToolExecutor) sessionOf(in agentrun.StepToolsInput) string {
	if in.Session != nil && in.Session.ID != "" {
		return in.Session.ID
	}
	if in.Run != nil {
		return in.Run.SessionID
	}
	return ""
}

// stepEntries lists one step's tool entries of run in call (sequence) order.
func (x *ClaimToolExecutor) stepEntries(ctx context.Context, run *models.AgentRun, sessionID, stepID string) ([]*models.SessionEntry, error) {
	entries, err := x.listToolEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, SessionID: sessionID, RunID: run.ID, StepID: stepID, Types: []models.SessionEntryType{models.SessionEntryTool},
	})
	if err != nil {
		return nil, fmt.Errorf("load tool entries of step %s: %w", stepID, err)
	}
	return entries, nil
}

var openToolEntryStates = []models.ToolEntryState{
	models.ToolEntryPending, models.ToolEntryRunning, models.ToolEntryAwaitingApproval, models.ToolEntryAwaitingChild, models.ToolEntryAwaitingEvent,
}

// openEntries lists the Run's open tool entries in sequence order.
func (x *ClaimToolExecutor) openEntries(ctx context.Context, run *models.AgentRun, sessionID string) ([]*models.SessionEntry, error) {
	entries, err := x.listToolEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, SessionID: sessionID, RunID: run.ID,
		Types: []models.SessionEntryType{models.SessionEntryTool}, States: openToolEntryStates,
	})
	if err != nil {
		return nil, fmt.Errorf("load open tool entries: %w", err)
	}
	return entries, nil
}

func (x *ClaimToolExecutor) listToolEntries(ctx context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
	var out []*models.SessionEntry
	filter.Limit = models.MaxSessionEntryListLimit
	for {
		page, err := x.cfg.Sessions.ListAgentSessionEntries(ctx, filter)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < models.MaxSessionEntryListLimit {
			break
		}
		next := page[len(page)-1].Sequence
		if next <= filter.AfterSequence {
			return nil, errors.New("session entries are not in sequence order")
		}
		filter.AfterSequence = next
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

func (x *ClaimToolExecutor) stepStillOpen(ctx context.Context, run *models.AgentRun, sessionID, stepID string) bool {
	entries, err := x.stepEntries(ctx, run, sessionID, stepID)
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if entry.State.Open() {
			return true
		}
	}
	return false
}
