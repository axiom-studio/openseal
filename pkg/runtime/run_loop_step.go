package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// One step of the loop (SPEC §3.1 phases 0-11).

type stepResultKind int

const (
	stepNext          stepResultKind = iota // the Run keeps running; Drive starts the next step
	stepFinalRejected                       // as stepNext, and the step's final answer was rejected
	stepExit                                // the claim ends with exit
)

type stepResult struct {
	kind     stepResultKind
	produced bool // the producer was called in this claim (fairness count)
	exit     RunLoopExit
}

func exitResult(exit RunLoopExit) stepResult { return stepResult{kind: stepExit, exit: exit} }

func exitResultPtr(exit *RunLoopExit) stepResult { return exitResult(*exit) }

// stepState is what the loop knows about the running step.
type stepState struct {
	lease *stepLease
	step  *AgentTurn // the finished row once finished; the begun row before
	mode  agentrun.StepMode

	started  time.Time
	stepCtx  context.Context // canceled on step lease loss
	durCtx   context.Context // stepCtx bounded by the duration budget
	chargeMS int64

	called    bool // the producer ran in this claim
	produced  bool // the step ran a producer (Turns budget dimension)
	finished  bool
	applied   bool
	usage     TurnUsage
	hostCalls int
	// compaction is the usage of this step's compaction calls (phase 4 and
	// the forced retry). It is added to the step row's usage; the compactor
	// already charged the bounded budget for it.
	compaction TurnUsage

	path        *AgentSessionPath
	output      *AssistantEntry // the step's model output
	assistant   *SessionEntry   // its stored entry (nil without a session)
	entries     []*SessionEntry // this step's tool entries
	finalEntry  string          // id of this step's final-tool entry, if any
	settle      bool            // a settle-phase call is pending (phase 10)
	kernelState map[string]interface{}

	preview previewTracker
}

// current is the latest row of the running step (the heartbeat renews it).
func (st *stepState) current() *AgentTurn {
	if st.finished || st.lease == nil {
		return st.step
	}
	return st.lease.get()
}

// ---------------------------------------------------------------------------
// Step lease

// stepLease serializes every write to the running step row in this process:
// the heartbeat, RecordStepModel, FinishTurn and ReleaseTurn. Each takes mu,
// uses turn.Revision as ExpectedRevision and replaces turn with the result.
type stepLease struct {
	mu   sync.Mutex
	turn *AgentTurn
	lost atomic.Bool

	stopOnce sync.Once
	stop     context.CancelFunc
	done     chan struct{}
}

func (s *stepLease) get() *AgentTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// write runs fn on the latest row under mu and stores its result.
func (s *stepLease) write(fn func(turn *AgentTurn) (*AgentTurn, error)) (*AgentTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := fn(s.turn)
	if err != nil {
		return nil, err
	}
	if next != nil {
		s.turn = next
	}
	return s.turn, nil
}

// startHeartbeat renews the step lease every duration/3. A failed renewal
// marks the lease lost and cancels the step context.
func (s *stepLease) startHeartbeat(ctx context.Context, cancelStep context.CancelFunc, turns *AgentTurnService, duration time.Duration, workerID string) {
	hbCtx, stop := context.WithCancel(ctx)
	s.stop = stop
	s.done = make(chan struct{})
	interval := duration / 3
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_, err := s.write(func(turn *AgentTurn) (*AgentTurn, error) {
					return turns.RenewTurn(hbCtx, turn.Scope, turn.ID, workerID, duration)
				})
				if err != nil {
					if hbCtx.Err() != nil {
						return
					}
					s.lost.Store(true)
					cancelStep()
					return
				}
			}
		}
	}()
}

func (s *stepLease) stopHeartbeat() {
	s.stopOnce.Do(func() {
		if s.stop != nil {
			s.stop()
			<-s.done
		}
	})
}

// ---------------------------------------------------------------------------
// runStep

// runStep runs one step: a new row, or the running row reconcile claimed.
func (d *drive) runStep(ctx context.Context, resumed *resumedStep) stepResult {
	mode := agentrun.StepModeNormal
	if conversation.RunExplainingFailure(d.run) {
		mode = agentrun.StepModeExplainFailure
	}
	var step *AgentTurn
	if resumed != nil {
		step = resumed.step
	} else {
		begun, err := d.l.turns.BeginTurn(ctx, BeginAgentTurnRequest{
			Scope: d.run.Scope, RunID: d.run.ID, SessionID: d.sessionID(), Producer: d.binding.Producer.Kind(),
			DefinitionID: d.binding.DefinitionID, DefinitionVersion: d.binding.DefinitionVersion,
			ModelProvider: d.binding.ModelProvider, Model: d.binding.Model, InputContextRefs: d.binding.InputContextRefs,
			WorkerID: d.workerID, LeaseDuration: d.l.config.StepLeaseDuration,
		})
		if err != nil {
			// ErrActiveTurnExists means reconcile was skipped: a programming
			// error. The Run stays running; its lease expiry recovers it.
			return exitResultPtr(d.writeErrorExit(err))
		}
		step = begun
	}
	st := &stepState{lease: &stepLease{turn: step}, step: step, mode: mode, started: d.l.now()}
	stepCtx, cancelStep := context.WithCancel(ctx)
	defer cancelStep()
	st.lease.startHeartbeat(stepCtx, cancelStep, d.l.turns, d.l.config.StepLeaseDuration, d.workerID)
	defer st.lease.stopHeartbeat()
	defer d.releaseUnfinished(ctx, st)
	if d.l.actions != nil {
		closeNested := d.l.actions.OpenNestedActionTurn(cloneAgentRun(d.run), cloneAgentTurn(step), d.binding)
		defer closeNested()
	}
	stepCtx = d.observeStepProgress(stepCtx, st)
	stepCtx = d.observeStepPreview(stepCtx, st)
	st.stepCtx = stepCtx
	res := d.stepPhases(ctx, st, resumed)
	res.produced = st.called
	return res
}

func (d *drive) sessionID() string {
	if d.session == nil {
		return ""
	}
	return d.session.ID
}

// releaseUnfinished gives up the step lease of a step that is still running
// when runStep returns (lease loss, an infrastructure error), so the next
// claim resumes it at once instead of waiting for the lease to expire.
func (d *drive) releaseUnfinished(ctx context.Context, st *stepState) {
	if st.finished {
		return
	}
	st.lease.stopHeartbeat()
	if st.lease.lost.Load() {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), runLoopCleanupTimeout)
	defer cancel()
	_, err := st.lease.write(func(turn *AgentTurn) (*AgentTurn, error) {
		return d.l.turns.ReleaseTurn(cleanup, turn.Scope, turn.ID, turn.Revision, d.workerID)
	})
	if err != nil && !errors.Is(err, ErrTurnLeaseHeld) && !errors.Is(err, ErrLeaseLost) {
		d.l.logger.Warnw("release unfinished step", "run", d.run.ID, "step", st.step.ID, "error", err)
	}
}

// stepInput is the producer/preparer/compactor view of the step.
func (d *drive) stepInput(st *stepState, attempt int) agentrun.StepInput {
	return agentrun.StepInput{
		Run: cloneAgentRun(d.run), Step: cloneAgentTurn(st.current()), Session: cloneAgentSession(d.session),
		Path: st.path, Binding: d.binding, Mode: st.mode, Attempt: attempt, WorkerID: d.workerID,
	}
}

// toolsInput is the executor's view of the step.
func (d *drive) toolsInput(st *stepState, entries []*SessionEntry) agentrun.StepToolsInput {
	return agentrun.StepToolsInput{
		Run: cloneAgentRun(d.run), Step: cloneAgentTurn(st.current()), Session: cloneAgentSession(d.session),
		Path: st.path, Binding: d.binding, WorkerID: d.workerID, Assistant: st.assistant, Entries: entries,
	}
}

func (d *drive) stepPhases(ctx context.Context, st *stepState, resumed *resumedStep) stepResult {
	if err := d.loadPath(ctx, st); err != nil {
		return d.storeError(ctx, st, err, StepFailurePreparationFailed)
	}
	// Phase 1: budget and duration.
	durCtx, cancelDuration, chargeMS, ok := stepDurationDeadline(st.stepCtx, d.run, st.step.ID)
	defer cancelDuration()
	st.durCtx, st.chargeMS = durCtx, chargeMS
	if !ok {
		return d.durationExpired(ctx, st, 0)
	}
	if res, done := d.reserveBudget(ctx, st); done {
		return res
	}
	if resumed != nil && resumed.assistant != nil {
		return d.resumeRecordedStep(ctx, st, resumed.assistant)
	}
	// Phase 2: open tool entries of earlier steps.
	if st.mode == agentrun.StepModeNormal {
		if res, done := d.resumeOpenTools(ctx, st); done {
			return res
		}
	}
	// Phase 3: steering, preparers, one context append.
	early, err := d.appendContext(ctx, st)
	if err != nil {
		if errors.Is(err, ErrChannelWatermarkMoved) {
			return d.hostUnavailable(ctx, st, err)
		}
		return d.stepError(ctx, st, err, StepFailurePreparationFailed)
	}
	if early != nil {
		return d.finishLoopFinal(ctx, st, early)
	}
	// Phase 4: compaction.
	if st.mode == agentrun.StepModeNormal && d.binding.Compactor != nil {
		if d.binding.Compactor.Due(d.stepInput(st, 0)) {
			usage, err := d.binding.Compactor.CompactIfNeeded(st.durCtx, d.stepInput(st, 0))
			st.addCompaction(usage)
			if err != nil {
				return d.stepError(ctx, st, err, StepFailureProducerFailed)
			}
			if err := d.afterCompaction(ctx, st); err != nil {
				return d.stepError(ctx, st, err, StepFailureProducerFailed)
			}
		}
	}
	// Phase 5: produce.
	out, res, done := d.produce(ctx, st)
	if done {
		return res
	}
	// Phase 7: record.
	if d.binding.Producer.Kind() == StepProducerModel {
		if err := d.recordModelStep(ctx, st, out); err != nil {
			return d.stepError(ctx, st, err, StepFailureHostConfiguration)
		}
		return d.inlinePhase(ctx, st)
	}
	return d.deterministicStep(ctx, st, out)
}

func (d *drive) loadPath(ctx context.Context, st *stepState) error {
	if d.session == nil {
		st.path = nil
		return nil
	}
	path, err := d.l.sessions.LoadAgentSessionPath(ctx, d.run.Scope, d.session.ID)
	if err != nil {
		return err
	}
	st.path = path
	if path.Session != nil {
		d.session = path.Session
		d.binding.Session = path.Session
	}
	return nil
}

// reserveBudget is phase 1 for bounded Runs.
func (d *drive) reserveBudget(ctx context.Context, st *stepState) (stepResult, bool) {
	if d.run.Budget == nil {
		return stepResult{}, false
	}
	for attempt := 0; ; attempt++ {
		run, admission, err := reserveStepBudget(st.durCtx, d.l.activity, d.l.now(), d.run, st.current(), d.binding, d.stepInput(st, 0), d.workerID)
		if errors.Is(err, ErrRevisionConflict) && attempt < maximumRunWriteAttempts {
			if _, err := d.reloadRun(ctx); err != nil {
				return d.storeError(ctx, st, err, StepFailurePreparationFailed), true
			}
			continue
		}
		if err != nil {
			// A refused admission is not an error (admission != nil below);
			// an error here is the reservation write or its planning.
			return d.storeError(ctx, st, err, StepFailurePreparationFailed), true
		}
		if admission == nil {
			d.run = run
			return stepResult{}, false
		}
		if err := d.finishStep(ctx, st, stepBudgetAdmissionFinish(st.current(), d.workerID, *admission)); err != nil {
			return exitResultPtr(d.writeErrorExit(err)), true
		}
		pause := stepBudgetPause(d.run, d.workerID, stepBudgetAdmissionSummary, admission, "")
		updated, err := d.commitRun(ctx, st, pause, false)
		if err != nil {
			return exitResultPtr(d.writeErrorExit(err)), true
		}
		return exitResultPtr(d.statusExit(updated)), true
	}
}

// durationExpired cancels a step the duration budget cut (before it started
// when elapsed is 0) and pauses the Run.
func (d *drive) durationExpired(ctx context.Context, st *stepState, elapsedMS int64) stepResult {
	finish := stepDurationExpiredFinish(st.current(), d.workerID, st.usage, elapsedMS, st.chargeMS)
	finish.OutputSummary = ""
	st.usage = finish.Usage
	if err := d.finishStep(ctx, st, finish); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	summary := stepDurationExceededSummary
	if elapsedMS == 0 && st.chargeMS == 0 {
		summary = stepDurationExpiredSummary
	}
	run, err := d.commitRun(ctx, st, stepBudgetPause(d.run, d.workerID, summary, nil, "duration_ms"), false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	return exitResultPtr(d.statusExit(run))
}

// resumeOpenTools is phase 2: the session holds open tool entries of this
// Run from earlier steps (an approved call, a finished child, a settled card,
// a crashed running entry).
func (d *drive) resumeOpenTools(ctx context.Context, st *stepState) (stepResult, bool) {
	if d.binding.Tools == nil {
		return stepResult{}, false
	}
	open, err := d.l.sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: d.run.Scope, RunID: d.run.ID, States: models.OpenToolEntryStateValues(), Limit: openToolEntryPage,
	})
	if err != nil {
		return d.storeError(ctx, st, err, StepFailurePreparationFailed), true
	}
	if len(open) == 0 {
		return stepResult{}, false
	}
	out, err := d.binding.Tools.ResumeOpenTools(st.durCtx, d.toolsInput(st, open))
	if res, done := d.toolOutcome(ctx, st, out, err); done {
		return res, true
	}
	if !st.settle {
		// Everything settled; the step goes on to its model call. A
		// resumed approval may have moved the Run.
		if _, err := d.reloadRun(ctx); err != nil {
			return d.storeError(ctx, st, err, StepFailurePreparationFailed), true
		}
		if err := d.loadPath(ctx, st); err != nil {
			return d.storeError(ctx, st, err, StepFailurePreparationFailed), true
		}
		return stepResult{}, false
	}
	return d.finishAndSettle(ctx, st), true
}

// resumeRecordedStep continues a running step whose assistant entry an
// earlier claim already appended (D14): the model is not called again.
func (d *drive) resumeRecordedStep(ctx context.Context, st *stepState, assistant *SessionEntry) stepResult {
	st.assistant = assistant
	st.produced = true
	a := assistant.Assistant
	st.output = a
	if len(a.ToolCalls) > 0 {
		entries, err := d.l.sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
			Scope: d.run.Scope, StepID: st.step.ID, Types: []SessionEntryType{SessionEntryTool}, Limit: openToolEntryPage,
		})
		if err != nil {
			return d.storeError(ctx, st, err, StepFailurePreparationFailed)
		}
		st.entries = entries
		st.finalEntry = finalToolEntryID(entries)
	}
	if st.current().AssistantEntryID == "" {
		usage := modelstep.TurnUsageFromModelStep(a.Usage)
		st.usage = usage
		if err := d.recordStep(ctx, st, string(a.Provider), a.Model, a, usage, nil); err != nil {
			return d.stepError(ctx, st, err, StepFailureHostConfiguration)
		}
	} else {
		st.usage = st.current().Usage
	}
	if len(a.ToolCalls) == 0 {
		return d.inlinePhase(ctx, st)
	}
	if res, done := d.resumeOpenTools(ctx, st); done {
		return res
	}
	return d.finishAndSettle(ctx, st)
}

// appendContext is phase 3. It returns a preparer's EarlyFinal.
func (d *drive) appendContext(ctx context.Context, st *stepState) (*agentrun.LoopFinal, error) {
	if d.session == nil {
		return nil, nil
	}
	preparers := d.binding.Preparers
	if st.mode == agentrun.StepModeExplainFailure {
		note := conversation.FailureExplanationNote(d.session.ID, d.run.ID, st.step.ID, d.run.FailureExplanation)
		note.Scope, note.Step = d.run.Scope, st.step.Sequence
		note.Tokens = modelstep.EstimateEntryTokens(note)
		preparers = []agentrun.StepPreparer{fixedEntriesPreparer{entries: []*SessionEntry{note}}}
	}
	for attempt := 0; ; attempt++ {
		result, err := appendStepContext(st.durCtx, d.l.sessions, d.stepInput(st, 0), preparers, d.l.now())
		if errors.Is(err, ErrSessionMoved) && attempt == 0 {
			if err := d.followSession(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if result.Session != nil {
			d.session = result.Session
			d.binding.Session = result.Session
		}
		if result.CursorMoved {
			if _, err := d.reloadRun(ctx); err != nil {
				return nil, err
			}
		}
		if err := d.loadPath(ctx, st); err != nil {
			return nil, err
		}
		return result.EarlyFinal, nil
	}
}

// fixedEntriesPreparer contributes fixed entries to the context append (the
// explanation step's failure note).
type fixedEntriesPreparer struct{ entries []*SessionEntry }

func (p fixedEntriesPreparer) BeforeStep(context.Context, agentrun.StepInput, agentrun.SteeringPlan) (*agentrun.StepContextAppend, error) {
	return &agentrun.StepContextAppend{Entries: p.entries}, nil
}

// followSession re-binds the claim to the Run's session after
// ErrSessionMoved (promotion forked the Run into a child session).
func (d *drive) followSession(ctx context.Context) error {
	run, err := d.reloadRun(ctx)
	if err != nil {
		return err
	}
	if run.SessionID == "" || d.session != nil && run.SessionID == d.session.ID {
		return ErrSessionMoved
	}
	session, err := d.l.sessions.GetAgentSession(ctx, run.Scope, run.SessionID)
	if err != nil {
		return err
	}
	d.session = session
	d.binding.Session = session
	return nil
}

// afterCompaction reloads what a compaction changed: the session path and
// the Run (its compaction budget writes).
func (d *drive) afterCompaction(ctx context.Context, st *stepState) error {
	if _, err := d.reloadRun(ctx); err != nil {
		return err
	}
	return d.loadPath(ctx, st)
}

// produce is phases 5 and 6. On a context-length rejection of the first
// attempt it compacts (force) and retries once.
func (d *drive) produce(ctx context.Context, st *stepState) (agentrun.StepOutput, stepResult, bool) {
	var out agentrun.StepOutput
	var err error
	for attempt := 0; ; attempt++ {
		st.called = true
		out, err = d.binding.Producer.Produce(st.durCtx, d.stepInput(st, attempt))
		var failure *modelstep.TurnHostFailure
		if err == nil || !errors.As(err, &failure) || failure.Code != modelstep.TurnHostContextExceeded {
			break
		}
		if attempt > 0 || st.mode != agentrun.StepModeNormal || d.binding.Compactor == nil {
			if res, done := d.checkRunAfterProduce(ctx, st); done {
				return out, res, true
			}
			guard, _ := modelstep.GuardRunFailureFor(modelstep.TurnHostContextExceeded)
			return out, d.fail(ctx, st, agentrun.LoopFailure{Kind: guard.Kind, Code: guard.Code, Message: guard.Message}, err, StepFailureContextExceeded), true
		}
		usage, err := d.binding.Compactor.Compact(modelstep.WithCompactionPressure(st.durCtx, failure.ContextPressure), d.stepInput(st, attempt), true)
		st.addCompaction(usage)
		if err != nil {
			return out, d.stepError(ctx, st, err, StepFailureContextExceeded), true
		}
		if err := d.afterCompaction(ctx, st); err != nil {
			return out, d.stepError(ctx, st, err, StepFailureContextExceeded), true
		}
	}
	elapsed := d.l.now().Sub(st.started).Milliseconds()
	if out.Usage.Validate() == nil {
		st.usage = out.Usage
	}
	if st.usage.DurationMS < elapsed {
		st.usage.DurationMS = elapsed
	}
	// Phase 6: the Run may have ended (or moved) while the producer ran.
	if res, done := d.checkRunAfterProduce(ctx, st); done {
		return out, res, true
	}
	if stepDurationExpired(st.stepCtx, st.durCtx) {
		return out, d.durationExpired(ctx, st, elapsed), true
	}
	if err != nil {
		return out, d.stepError(ctx, st, err, StepFailureProducerFailed), true
	}
	if err := modelstep.ValidateStepOutput(d.binding.Producer.Kind(), out); err != nil {
		return out, d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: StepFailureProducerFailed,
			Message: "The agent returned a step the kernel could not accept.", Explainable: true,
		}, err, StepFailureProducerFailed), true
	}
	st.produced = true
	return out, stepResult{}, false
}

// checkRunAfterProduce re-reads the Run (phase 6). A terminal Run discards
// the step's output; a Run that is no longer this claim's loses the lease.
func (d *drive) checkRunAfterProduce(ctx context.Context, st *stepState) (stepResult, bool) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), runLoopCleanupTimeout)
	defer cancel()
	run, err := d.l.store.GetAgentRun(cleanup, d.run.Scope, d.run.ID)
	if err != nil {
		return exitResultPtr(d.errorExit(err)), true
	}
	if run == nil {
		return exitResultPtr(d.errorExit(ErrRunNotFound)), true
	}
	if isTerminalAgentRunStatus(run.Status) {
		d.run = run
		d.endStep(cleanup, st)
		return exitResult(RunLoopExit{Run: run, Reason: RunLoopExitEnded}), true
	}
	if run.Status != AgentRunStatusRunning || run.LeaseOwner != d.workerID || st.lease.lost.Load() || ctx.Err() != nil {
		d.run = run
		return exitResult(RunLoopExit{Run: run, Reason: RunLoopExitLeaseLost, Err: ErrLeaseLost}), true
	}
	return stepResult{}, false
}

// endStep finishes the step of a Run that became terminal as
// canceled/run_ended. The terminal close may already have finished it.
func (d *drive) endStep(ctx context.Context, st *stepState) {
	err := d.finishStep(ctx, st, FinishAgentTurnRequest{
		Status: AgentTurnStatusCanceled, FailureCode: agentrun.StepFailureRunEnded, Error: agentrun.TerminalCloseStepError,
	})
	if err != nil && !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrTurnLeaseHeld) {
		d.l.logger.Warnw("finish step of an ended run", "run", d.run.ID, "step", st.step.ID, "error", err)
	}
	st.finished = true
}

// ---------------------------------------------------------------------------
// Phase 7: record

// recordModelStep appends [assistant, tool(pending)×N] in one append, then
// records the step's model output on its row (narration, turn.narration).
func (d *drive) recordModelStep(ctx context.Context, st *stepState, out agentrun.StepOutput) error {
	a := out.Assistant
	st.output = a
	if d.session == nil {
		if len(a.ToolCalls) > 0 {
			return fmt.Errorf("%w: a sessionless binding cannot run tool calls", modelstep.ErrTurnHostConfiguration)
		}
		return d.recordStep(ctx, st, out.ModelProvider, out.Model, a, st.usage, out.SkillSelections)
	}
	step := st.current()
	appended, err := d.appendStepEntries(ctx, func(session *AgentSession) ([]*SessionEntry, error) {
		assistant := &SessionEntry{
			ID: SessionEntryID(session.ID, AssistantEntryKey(step.ID)), Scope: d.run.Scope, SessionID: session.ID,
			RunID: d.run.ID, StepID: step.ID, Step: step.Sequence, Type: SessionEntryAssistant, Assistant: a,
		}
		assistant.Tokens = modelstep.EstimateEntryTokens(assistant)
		entries := []*SessionEntry{assistant}
		if len(a.ToolCalls) == 0 {
			return entries, nil
		}
		tools, err := d.l.config.PlanToolEntries(d.binding, session, step, assistant, nil)
		if err != nil {
			return nil, err
		}
		if st.mode == agentrun.StepModeExplainFailure {
			// The explanation step is tool-free: its calls are refused as
			// they are appended, so none stays open in the session.
			refuseExplanationToolEntries(tools, d.l.now())
		}
		return append(entries, tools...), nil
	})
	if err != nil {
		return err
	}
	st.assistant = appended[0]
	st.entries = appended[1:]
	st.finalEntry = finalToolEntryID(st.entries)
	provider, model := out.ModelProvider, out.Model
	if provider == "" && model == "" {
		provider, model = string(a.Provider), a.Model
	}
	if err := d.recordStep(ctx, st, provider, model, a, st.usage, out.SkillSelections); err != nil {
		return err
	}
	if len(a.ToolCalls) > 0 {
		d.resetPreview(ctx, st)
	}
	return nil
}

// explanationToolRefusal is the result of a tool call made by the
// tool-free explanation step.
const explanationToolRefusal = "refused: no tools can run while the agent explains a failure"

// refuseExplanationToolEntries ends the planned tool entries of an
// explanation step with a refused result.
func refuseExplanationToolEntries(entries []*SessionEntry, now time.Time) {
	for _, entry := range entries {
		if entry == nil || entry.Tool == nil || entry.State.Terminal() {
			continue
		}
		completed := now
		entry.State = ToolEntryError
		entry.Tool.Wait = nil
		entry.Tool.Parked = false
		entry.Tool.CompletedAt = &completed
		entry.Tool.Result = &ToolResult{
			Status: ToolResultRefused, IsError: true,
			Content: []ContentPart{{Type: ContentPartText, Text: explanationToolRefusal}},
		}
	}
}

// appendStepEntries appends the entries build makes for the current session,
// fenced by the Run lease. After ErrSessionMoved (promotion) it follows the
// Run to its new session and appends there once. It returns the stored
// entries in request order.
func (d *drive) appendStepEntries(ctx context.Context, build func(session *AgentSession) ([]*SessionEntry, error)) ([]*SessionEntry, error) {
	for attempt := 0; ; attempt++ {
		entries, err := build(d.session)
		if err != nil {
			return nil, err
		}
		now := d.l.now()
		result, err := d.l.sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
			Scope: d.run.Scope, SessionID: d.session.ID, RunID: d.run.ID,
			Lease: &AgentRunLeaseGuard{WorkerID: d.workerID, Now: now}, Entries: entries, Now: now,
		})
		if errors.Is(err, ErrSessionMoved) && attempt == 0 {
			if err := d.followSession(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if result.Session != nil {
			d.session = result.Session
			d.binding.Session = result.Session
		}
		if len(result.Entries) == len(entries) {
			return result.Entries, nil
		}
		return entries, nil
	}
}

// recordStep writes RecordStepModel for the step. Narration is the assistant
// text of a step that has tool calls (D11).
func (d *drive) recordStep(ctx context.Context, st *stepState, provider, model string, a *AssistantEntry, usage TurnUsage, selections []HostedSkillSelection) error {
	record := StepModelRecord{WorkerID: d.workerID, ModelProvider: provider, Model: model, Usage: usage, SkillSelections: selections}
	if st.assistant != nil {
		record.AssistantEntryID = st.assistant.ID
	}
	if a != nil {
		record.ToolCallCount = len(a.ToolCalls)
		record.StopReason = string(a.StopReason)
		if record.ToolCallCount > 0 {
			record.OutputSummary = a.Text
		}
		record.Final = record.ToolCallCount == 0 || st.finalEntry != ""
	}
	_, err := st.lease.write(func(turn *AgentTurn) (*AgentTurn, error) {
		record.ExpectedRevision = turn.Revision
		return d.l.turns.RecordStepModel(ctx, turn.Scope, turn.ID, record)
	})
	return err
}

// recordDeterministicStep records a deterministic step (no assistant entry).
func (d *drive) recordDeterministicStep(ctx context.Context, st *stepState, out agentrun.StepOutput) error {
	record := StepModelRecord{
		WorkerID: d.workerID, ModelProvider: out.ModelProvider, Model: out.Model,
		ToolCallCount: len(out.ToolCalls), Usage: st.usage, SkillSelections: out.SkillSelections,
	}
	_, err := st.lease.write(func(turn *AgentTurn) (*AgentTurn, error) {
		record.ExpectedRevision = turn.Revision
		return d.l.turns.RecordStepModel(ctx, turn.Scope, turn.ID, record)
	})
	return err
}

// finalToolEntryID is the id of the final-tool entry among a step's tool
// entries, or "".
func finalToolEntryID(entries []*SessionEntry) string {
	for _, entry := range entries {
		if entry != nil && entry.Tool != nil && IsFinalTool(ControlToolName(entry.Tool.Name)) {
			return entry.ID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Phases 8-11

// inlinePhase is phase 8 for a model step, then phases 9-11.
func (d *drive) inlinePhase(ctx context.Context, st *stepState) stepResult {
	assistant := st.output
	if assistant == nil || len(assistant.ToolCalls) == 0 {
		text := ""
		if assistant != nil {
			text = assistant.Text
		}
		return d.plainTextFinal(ctx, st, text)
	}
	if st.mode == agentrun.StepModeExplainFailure {
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: StepFailureProducerFailed,
			Message: "The agent called tools while explaining a failure.",
		}, nil, StepFailureProducerFailed)
	}
	return d.executeTools(ctx, st)
}

// executeTools runs the inline phase on the step's tool entries, then
// phases 9-11.
func (d *drive) executeTools(ctx context.Context, st *stepState) stepResult {
	if d.binding.Tools == nil {
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: StepFailureHostConfiguration,
			Message: "The agent called tools, but this run has no tool executor.",
		}, nil, StepFailureHostConfiguration)
	}
	out, err := d.binding.Tools.ExecuteStepTools(st.durCtx, d.toolsInput(st, st.entries))
	if res, done := d.toolOutcome(ctx, st, out, err); done {
		return res
	}
	return d.finishAndSettle(ctx, st)
}

// plainTextFinal is phase 8 for a step with no tool calls.
func (d *drive) plainTextFinal(ctx context.Context, st *stepState, text string) stepResult {
	if st.mode == agentrun.StepModeExplainFailure {
		text = strings.TrimSpace(text)
		if text == "" {
			return d.fail(ctx, st, agentrun.LoopFailure{
				Kind: conversation.RunFailureKindExecution, Code: StepFailureProducerFailed,
				Message: "The agent did not explain the failure.",
			}, nil, StepFailureProducerFailed)
		}
		return d.finishFinal(ctx, st, &agentrun.FinalAnswer{
			StepID: st.step.ID, PlainText: true, Answer: text,
			Output: map[string]interface{}{"summary": text},
		}, "")
	}
	if d.session == nil {
		text = strings.TrimSpace(text)
		return d.finishFinal(ctx, st, &agentrun.FinalAnswer{
			StepID: st.step.ID, PlainText: true, Answer: text,
			Output: map[string]interface{}{"summary": text},
		}, "")
	}
	cc := NewControlContext(d.toolsInput(st, nil), nil)
	result, err := d.l.config.PlainTextFinal(st.durCtx, cc, text)
	if err != nil {
		return d.stepError(ctx, st, err, StepFailureProducerFailed)
	}
	if result.Run != nil && result.Run.Revision >= d.run.Revision {
		d.run = cloneAgentRun(result.Run)
	}
	switch result.Kind {
	case ControlFinal:
		return d.finishFinal(ctx, st, result.Final, "")
	case ControlContinue:
		return d.continueAfter(ctx, st, stepFinalRejected)
	case ControlFailed:
		return d.fail(ctx, st, *result.Failure, nil, "")
	case ControlParked:
		return d.park(ctx, st, *result.Park)
	case ControlEnded:
		return d.endedByTool(ctx, st, result.Run)
	}
	return d.stepError(ctx, st, fmt.Errorf("plain-text final returned %q", result.Kind), StepFailureProducerFailed)
}

// deterministicStep handles a deterministic producer's output (phases 7-11).
func (d *drive) deterministicStep(ctx context.Context, st *stepState, out agentrun.StepOutput) stepResult {
	st.kernelState = out.KernelState
	if err := d.recordDeterministicStep(ctx, st, out); err != nil {
		return d.stepError(ctx, st, err, StepFailureHostConfiguration)
	}
	switch {
	case out.Failure != nil:
		return d.fail(ctx, st, *out.Failure, nil, "")
	case out.Final != nil:
		return d.finishLoopFinal(ctx, st, out.Final)
	case out.Park != nil:
		return d.park(ctx, st, *out.Park)
	}
	if d.session == nil {
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: StepFailureHostConfiguration,
			Message: "The run has tool calls to execute but no session.",
		}, nil, StepFailureHostConfiguration)
	}
	step := st.current()
	entries, err := d.appendStepEntries(ctx, func(session *AgentSession) ([]*SessionEntry, error) {
		return d.l.config.PlanToolEntries(d.binding, session, step, nil, out.ToolCalls)
	})
	if err != nil {
		return d.stepError(ctx, st, err, StepFailureHostConfiguration)
	}
	st.entries = entries
	st.finalEntry = finalToolEntryID(entries)
	return d.executeTools(ctx, st)
}

// toolOutcome branches on an executor outcome (phases 2, 8 and 10). done is
// false when the step goes on (st.settle tells whether a settle-phase call is
// pending).
func (d *drive) toolOutcome(ctx context.Context, st *stepState, out agentrun.StepToolsOutcome, err error) (stepResult, bool) {
	st.hostCalls += out.HostToolCalls
	if out.Run != nil && out.Run.Revision >= d.run.Revision {
		d.run = cloneAgentRun(out.Run)
	}
	if err != nil {
		return d.stepError(ctx, st, err, ""), true
	}
	switch {
	case out.Failure != nil:
		return d.fail(ctx, st, *out.Failure, nil, ""), true
	case out.BudgetExhausted != nil:
		return d.budgetExhausted(ctx, st, out.BudgetExhausted), true
	case out.Final != nil:
		return d.finishFinal(ctx, st, out.Final, ""), true
	case out.Park != nil:
		return d.park(ctx, st, *out.Park), true
	case out.Run != nil && isTerminalAgentRunStatus(out.Run.Status):
		// A control tool ended the Run in its own transaction (A4).
		return d.endedByTool(ctx, st, out.Run), true
	}
	st.settle = out.SettlePending
	return stepResult{}, false
}

// finishAndSettle is phases 9-11 after an inline phase that did not end the
// step: finish + apply, then the single settle-phase call.
func (d *drive) finishAndSettle(ctx context.Context, st *stepState) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	run, err := d.commitRun(ctx, st, RunTransitionRequest{Status: AgentRunStatusRunning}, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if run.Status != AgentRunStatusRunning {
		return exitResultPtr(d.statusExit(run))
	}
	if st.settle {
		st.settle = false
		out, err := d.binding.Tools.SettleStep(ctx, d.toolsInput(st, st.entries))
		if res, done := d.toolOutcome(ctx, st, out, err); done {
			return res
		}
	}
	if d.finalRejected(ctx, st) {
		return stepResult{kind: stepFinalRejected}
	}
	return stepResult{kind: stepNext}
}

// finalRejected reports whether this step's final-tool entry ended in an
// error result (a rejection counted toward maximumFinalRejections).
func (d *drive) finalRejected(ctx context.Context, st *stepState) bool {
	if st.finalEntry == "" {
		return false
	}
	entry, err := d.l.sessions.GetSessionEntry(ctx, d.run.Scope, st.finalEntry)
	return err == nil && entry != nil && entry.State == ToolEntryError
}

// continueAfter applies the step (if it is not yet) with the Run running and
// returns kind.
func (d *drive) continueAfter(ctx context.Context, st *stepState, kind stepResultKind) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if st.applied {
		return stepResult{kind: kind}
	}
	run, err := d.commitRun(ctx, st, RunTransitionRequest{Status: AgentRunStatusRunning}, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if run.Status != AgentRunStatusRunning {
		return exitResultPtr(d.statusExit(run))
	}
	return stepResult{kind: kind}
}

// endedByTool finishes the step of a Run a tool ended in its own transaction.
func (d *drive) endedByTool(ctx context.Context, st *stepState, run *AgentRun) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil && !errors.Is(err, ErrTurnLeaseHeld) && !errors.Is(err, ErrRevisionConflict) {
		return exitResultPtr(d.writeErrorExit(err))
	}
	if run != nil {
		d.run = cloneAgentRun(run)
	}
	return exitResultPtr(d.statusExit(d.run))
}

// budgetExhausted pauses the Run on an executor's budget exit.
func (d *drive) budgetExhausted(ctx context.Context, st *stepState, b *agentrun.BudgetExhaustion) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	dimension := ""
	if len(b.Dimensions) > 0 {
		dimension = b.Dimensions[0]
	}
	pause := stepBudgetPause(d.run, d.workerID, b.Summary, b.Admission, dimension)
	if b.ActionCallID != "" {
		pause.Payload["actionCallId"] = b.ActionCallID
	}
	run, err := d.commitRun(ctx, st, pause, false)
	if err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	return exitResultPtr(d.statusExit(run))
}

// park is §3.2. A committed park (fork, run_operation, delegate) already
// moved the Run; the loop only finishes the step. Every other park is a CAS
// transition on the revision read after the step's last write, guarded by
// the intervention count; a conflict re-plans instead of re-applying.
func (d *drive) park(ctx context.Context, st *stepState, park agentrun.LoopPark) stepResult {
	if err := d.finishCompleted(ctx, st); err != nil {
		return exitResultPtr(d.writeErrorExit(err))
	}
	for replans := 0; ; replans++ {
		if park.Committed {
			run, err := d.reloadRun(ctx)
			if err != nil {
				return exitResultPtr(d.errorExit(err))
			}
			return exitResultPtr(d.statusExit(run))
		}
		count := len(d.run.PendingInterventions)
		summary := park.Summary
		if summary == "" {
			summary = fmt.Sprintf("Run is %s", strings.ReplaceAll(string(park.Status), "_", " "))
		}
		run, err := d.commitRun(ctx, st, RunTransitionRequest{
			Status: park.Status, WakeCondition: park.Wake, Summary: summary, RequireInterventionCount: &count,
		}, true)
		if err == nil {
			return exitResultPtr(d.statusExit(run))
		}
		if !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrSteeringArrived) {
			return exitResultPtr(d.writeErrorExit(err))
		}
		if replans+1 >= maximumParkReplans {
			return exitResult(d.requeue(ctx, st, nil, runRequeueReasonParkReplan, "Run requeued after its park was re-planned too often"))
		}
		reloaded, err := d.reloadRun(ctx)
		if err != nil {
			return exitResultPtr(d.errorExit(err))
		}
		if reloaded.Status != AgentRunStatusRunning || reloaded.LeaseOwner != d.workerID {
			return exitResultPtr(d.statusExit(reloaded))
		}
		var next *agentrun.LoopPark
		if d.binding.Tools != nil {
			next, err = d.binding.Tools.ReplanPark(ctx, d.toolsInput(st, st.entries))
			if err != nil {
				return d.stepError(ctx, st, err, "")
			}
		}
		if next == nil {
			// The awaited decision or card is already settled: continue.
			return d.continueAfter(ctx, st, stepNext)
		}
		park = *next
	}
}

// ---------------------------------------------------------------------------
// Step writes

// finishStep finishes the running step (phase 9 and every exit). The
// heartbeat stops first, so the finish holds the latest revision.
func (d *drive) finishStep(ctx context.Context, st *stepState, req FinishAgentTurnRequest) error {
	if st.finished {
		return nil
	}
	st.lease.stopHeartbeat()
	if st.lease.lost.Load() {
		return ErrTurnLeaseHeld
	}
	if req.Usage == (TurnUsage{}) {
		req.Usage = st.usage
	}
	req.Usage.InputTokens += st.compaction.InputTokens
	req.Usage.OutputTokens += st.compaction.OutputTokens
	req.Usage.CacheReadTokens += st.compaction.CacheReadTokens
	req.Usage.CacheWriteTokens += st.compaction.CacheWriteTokens
	req.Usage.Cost += st.compaction.Cost
	req.Usage.NativeOperations += st.hostCalls
	req.WorkerID = d.workerID
	finished, err := st.lease.write(func(turn *AgentTurn) (*AgentTurn, error) {
		req.ExpectedRevision = turn.Revision
		return d.l.turns.FinishTurn(context.WithoutCancel(ctx), turn.Scope, turn.ID, req)
	})
	if err != nil {
		return err
	}
	st.step = finished
	st.finished = true
	return nil
}

// addCompaction adds the usage of one compaction call to the step.
func (st *stepState) addCompaction(u ModelStepUsage) {
	st.compaction.InputTokens += int(u.InputTokens)
	st.compaction.OutputTokens += int(u.OutputTokens)
	st.compaction.CacheReadTokens += int(u.CacheReadTokens)
	st.compaction.CacheWriteTokens += int(u.CacheWriteTokens)
	if u.Cost > 0 {
		st.compaction.Cost += u.Cost
	}
}

func (d *drive) finishCompleted(ctx context.Context, st *stepState) error {
	if st.usage.DurationMS == 0 {
		st.usage.DurationMS = d.l.now().Sub(st.started).Milliseconds()
	}
	return d.finishStep(ctx, st, FinishAgentTurnRequest{Status: AgentTurnStatusCompleted})
}

// stepError classifies an error of a step phase. defaultCode is the step
// FailureCode of an unclassified error ("" = execution_failed).
func (d *drive) stepError(ctx context.Context, st *stepState, err error, defaultCode string) stepResult {
	var loopFailure *modelstep.LoopFailureError
	var hostFailure *modelstep.TurnHostFailure
	switch {
	case errors.Is(err, errRunEnded):
		if res, done := d.checkRunAfterProduce(ctx, st); done {
			return res
		}
		return exitResultPtr(d.writeErrorExit(err))
	case isLeaseLoss(err) || st.lease.lost.Load() || ctx.Err() != nil:
		if res, done := d.checkRunAfterProduce(ctx, st); done {
			return res
		}
		return exitResult(RunLoopExit{Run: d.run, Reason: RunLoopExitLeaseLost, Err: err})
	case st.durCtx != nil && stepDurationExpired(st.stepCtx, st.durCtx):
		return d.durationExpired(ctx, st, d.l.now().Sub(st.started).Milliseconds())
	case errors.Is(err, modelstep.ErrTurnHostUnavailable):
		return d.hostUnavailable(ctx, st, err)
	case errors.As(err, &loopFailure):
		return d.fail(ctx, st, loopFailure.Failure, err, loopFailure.Failure.Code)
	case errors.As(err, &hostFailure):
		code := conversation.RunFailureCodeOrEmpty(hostFailure.Code)
		if code == "" {
			code = StepFailureProducerFailed
		}
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: code, Message: hostFailure.Error(),
			Explainable: conversation.TurnFailureCanBeExplained(err, code),
		}, err, code)
	case errors.Is(err, modelstep.ErrTurnHostConfiguration):
		message := "The agent's step host is not configured correctly."
		if errors.Is(err, ErrInvalidModelStepRequest) {
			// The host is fine; the step built from the session was refused.
			message = "The agent's next step could not be prepared from this conversation."
		}
		return d.fail(ctx, st, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: StepFailureHostConfiguration,
			Message: message,
		}, err, StepFailureHostConfiguration)
	}
	code := defaultCode
	if code == "" {
		code = "execution_failed"
	}
	return d.fail(ctx, st, agentrun.LoopFailure{
		Kind: conversation.RunFailureKindExecution, Code: code, Message: "The requested work did not complete.",
		Explainable: conversation.TurnFailureCanBeExplained(err, code),
	}, err, code)
}

// storeError handles an error of a store read or of the budget reservation
// in phases 1-3. A kernel-classified error (lease loss, a host or loop
// failure, an exhausted budget, a missing or oversized session) goes through
// stepError. Any other error is infrastructure: the Run is left running, the
// step lease is released, and the Run's lease expiry lets a later claim
// resume it.
func (d *drive) storeError(ctx context.Context, st *stepState, err error, defaultCode string) stepResult {
	var loopFailure *modelstep.LoopFailureError
	var hostFailure *modelstep.TurnHostFailure
	switch {
	case errors.Is(err, errRunEnded), isLeaseLoss(err), st.lease.lost.Load(), ctx.Err() != nil,
		st.durCtx != nil && stepDurationExpired(st.stepCtx, st.durCtx),
		errors.Is(err, modelstep.ErrTurnHostUnavailable), errors.Is(err, modelstep.ErrTurnHostConfiguration),
		errors.As(err, &loopFailure), errors.As(err, &hostFailure),
		errors.Is(err, ErrAgentSessionNotFound), errors.Is(err, ErrAgentSessionPathTooLarge), errors.Is(err, ErrAgentSessionForkTooDeep):
		return d.stepError(ctx, st, err, defaultCode)
	case errors.Is(err, ErrBudgetExhausted):
		return d.stepError(ctx, st, err, StepFailureBudgetExhausted)
	}
	d.l.logger.Warnw("step store error; the run is left for lease recovery", "run", d.run.ID, "step", st.step.ID, "error", err)
	return exitResultPtr(d.errorExit(err))
}

// ---------------------------------------------------------------------------
// Observers (fenced by the step lease and the Run lease)

// stepFenced checks that the step is still this worker's running step and the
// Run still this claim's. Observers call it from host and executor
// goroutines, so it takes the ids captured when the step began and never
// reads the loop's d.run or st.
func (d *drive) stepFenced(ctx context.Context, scope Scope, runID, stepID string) error {
	current, err := d.l.store.GetAgentTurn(ctx, scope, stepID)
	if err != nil {
		return err
	}
	if current == nil || current.Status != AgentTurnStatusRunning || current.LeaseOwner != d.workerID ||
		current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(d.l.now()) {
		return ErrTurnLeaseHeld
	}
	active, err := d.l.store.GetAgentRun(ctx, scope, runID)
	if err != nil {
		return err
	}
	if active == nil || active.Status != AgentRunStatusRunning || active.LeaseOwner != "" && active.LeaseOwner != d.workerID {
		return ErrLeaseLost
	}
	return nil
}

// observeStepProgress records host and tool progress as turn.progress /
// turn.commentary activity of the step, deduplicating repeats.
func (d *drive) observeStepProgress(ctx context.Context, st *stepState) context.Context {
	var mu sync.Mutex
	lastSummary, lastKind, lastOrigin := "", "", ""
	run := cloneAgentRun(d.run)
	stepID := st.step.ID
	return WithTurnProgress(ctx, func(progressCtx context.Context, summary string) error {
		kind := "turn.progress"
		if IsTurnCommentary(progressCtx) {
			kind = "turn.commentary"
		}
		origin := TurnProgressOrigin(progressCtx)
		mu.Lock()
		defer mu.Unlock()
		if summary == lastSummary && kind == lastKind && origin == lastOrigin {
			return nil
		}
		if err := d.stepFenced(progressCtx, run.Scope, run.ID, stepID); err != nil {
			return err
		}
		event := &ActivityEvent{
			Scope: run.Scope, RunID: run.ID, TurnID: stepID, AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: teamIDForRun(run),
			EventType: kind, Summary: summary, Actor: d.actor(), Visibility: ActivityVisibilityScope,
		}
		if origin != "" {
			event.Payload = map[string]interface{}{"originRef": origin}
		}
		if _, err := d.l.activity.AppendActivity(progressCtx, event); err != nil {
			return err
		}
		lastSummary, lastKind, lastOrigin = summary, kind, origin
		return nil
	})
}

// previewTracker remembers the last answer-preview frame of the step, so a
// step that ends in tool calls can reset it (D11).
type previewTracker struct {
	mu        sync.Mutex
	attemptID string
	sequence  uint64
}

func (p *previewTracker) seen(preview TurnAnswerPreview) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if preview.AttemptID != p.attemptID || preview.Sequence > p.sequence {
		p.attemptID, p.sequence = preview.AttemptID, preview.Sequence
	}
}

func (p *previewTracker) last() (string, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attemptID, p.sequence
}

func (d *drive) observeStepPreview(ctx context.Context, st *stepState) context.Context {
	run := cloneAgentRun(d.run)
	stepID := st.step.ID
	tracker := &st.preview
	return WithTurnAnswerPreview(ctx, func(previewCtx context.Context, preview TurnAnswerPreview) error {
		if err := d.stepFenced(previewCtx, run.Scope, run.ID, stepID); err != nil {
			return err
		}
		if err := d.appendPreview(previewCtx, run, stepID, preview); err != nil {
			return err
		}
		tracker.seen(preview)
		return nil
	})
}

func (d *drive) appendPreview(ctx context.Context, run *AgentRun, stepID string, preview TurnAnswerPreview) error {
	_, err := d.l.activity.AppendActivity(ctx, &ActivityEvent{
		Scope: run.Scope, RunID: run.ID, TurnID: stepID, AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: teamIDForRun(run),
		EventType: "turn.answer_preview", Summary: "Reply preview updated", Actor: d.actor(), Visibility: ActivityVisibilityScope,
		Payload: map[string]interface{}{"attemptId": preview.AttemptID, "sequence": preview.Sequence, "text": preview.Text, "reset": preview.Reset},
	})
	return err
}

// resetPreview clears the answer preview of a step that ended in tool calls
// (same attempt id, next sequence). A lost reset is not retried: the next
// preview or the reply replaces it.
func (d *drive) resetPreview(ctx context.Context, st *stepState) {
	attemptID, sequence := st.preview.last()
	if attemptID == "" {
		return
	}
	reset := TurnAnswerPreview{AttemptID: attemptID, Sequence: sequence + 1, Reset: true}
	if err := d.appendPreview(ctx, d.run, st.step.ID, reset); err != nil {
		d.l.logger.Warnw("reset answer preview", "run", d.run.ID, "step", st.step.ID, "error", err)
		return
	}
	st.preview.seen(reset)
}
