package runloop

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

// RunLoop is the kernel-owned agent loop (D1): one claim of a Run runs steps
// until a final answer, a park, a fairness yield, a requeue, a budget pause or
// a failure. Each step is one agent_turns row; the model's view of the work is
// the Run's session log, never Run.Checkpoint.
//
// The step algorithm is SPEC §3.1; parks §3.2; the final chain §3.3; failure
// and explanation §3.5. The worker resolves the binding, holds the Run lease
// and runs terminal reporting after Drive returns.

const (
	// RunLoopMaxStepsPerClaim and RunLoopMaxClaimDuration bound one claim
	// (D13); the Run then yields and the next claim resumes from the log.
	RunLoopMaxStepsPerClaim = 50
	RunLoopMaxClaimDuration = 8 * time.Minute

	// maximumFinalRejections is the bound of consecutive final-answer
	// rejections (A9): the third fails the Run with final_answer_rejected.
	maximumFinalRejections = 3
	// maximumParkReplans bounds the park CAS re-plans of one step (§3.2).
	maximumParkReplans = 8
	// maximumRunWriteAttempts bounds the reload-and-retry of a Run write
	// after a revision conflict (heartbeats, approvals and catch-up bump the
	// revision between the read and the write).
	maximumRunWriteAttempts = 8

	defaultStepLeaseDuration = 30 * time.Second
	runLoopCleanupTimeout    = 5 * time.Second
	// openToolEntryPage bounds the open tool entries one resume reads.
	openToolEntryPage = 128
)

// Step FailureCode values written by the loop (SPEC §2.8). duration_budget,
// budget_exhausted and run_ended are declared beside their writers.
const (
	StepFailureHostUnavailable      = "host_unavailable"
	StepFailureHostConfiguration    = "host_configuration"
	StepFailureContextExceeded      = modelstep.TurnHostContextExceeded
	StepFailureProducerFailed       = "producer_failed"
	StepFailurePreparationFailed    = "step_preparation_failed"
	runFailureCodeFinalRejected     = "final_answer_rejected"
	runFailureCodePreparedRuntime   = "prepared_runtime_invalid"
	runFailureCodeFinalDelivery     = "final_delivery_failed"
	runFailureCodeOutputPublication = "output_publication_failed"
	runRequeueReasonParkReplan      = "park_replan_exhausted"
	runRequeueReasonStepLeaseHeld   = "step_lease_held"
	runRequeueReasonHostUnavailable = "host_unavailable"
)

// StepToolEntryPlanner builds the tool entries of one step (disp-1
// PlanToolEntries): pending entries with SideEffect, RepeatEffect and
// ArgumentsDigest set, and calls with ArgumentsError or oversize arguments
// already error/refused. For a model step assistant is the step's assistant
// entry and calls is nil; for a deterministic step assistant is nil and calls
// are the producer's calls (Directive kept on the entry). Entry ids are
// SessionEntryID(session, ToolEntryKey(step.ID, callID)).
type StepToolEntryPlanner func(binding *agentrun.TurnRunnerBinding, session *models.AgentSession, step *models.AgentTurn, assistant *models.SessionEntry, calls []agentrun.LoopToolCall) ([]*models.SessionEntry, error)

// PlainTextFinalizer judges the text of a model step that made no tool calls
// (CTL-14 ControlExecutor.PlainTextFinal): ControlFinal accepts it,
// ControlContinue means a final_rejection note was appended, ControlFailed
// fails the Run.
type PlainTextFinalizer func(ctx context.Context, cc agentrun.ControlContext, text string) (control.ControlResult, error)

type RunLoopConfig struct {
	StepLeaseDuration time.Duration // default 30s (AgentRunWorkerConfig.TurnLeaseDuration)
	MaxStepsPerClaim  int           // default RunLoopMaxStepsPerClaim
	MaxClaimDuration  time.Duration // default RunLoopMaxClaimDuration
	Now               func() time.Time
	// PlanToolEntries and PlainTextFinal are the dispatcher's and the
	// control section's parts of phases 7 and 8. Both are required.
	PlanToolEntries StepToolEntryPlanner
	PlainTextFinal  PlainTextFinalizer
}

type RunLoop struct {
	store    models.KernelStore
	sessions models.AgentSessionStore
	activity *runtimeactivity.RunActivityService
	turns    *runtimeactivity.AgentTurnService
	actions  *action.ActionCoordinator // nil: no nested-action window
	config   RunLoopConfig
	logger   *zap.SugaredLogger
}

// NewRunLoop builds the loop over the kernel store. The store must also be
// the session log (AgentSessionStore).
func NewRunLoop(store models.KernelStore, actions *action.ActionCoordinator, logger *zap.SugaredLogger, config RunLoopConfig) (*RunLoop, error) {
	if store == nil {
		return nil, errors.New("run loop needs a kernel store")
	}
	sessions, ok := store.(models.AgentSessionStore)
	if !ok {
		return nil, errors.New("run loop needs a kernel store with the agent session log")
	}
	if config.PlanToolEntries == nil || config.PlainTextFinal == nil {
		return nil, errors.New("run loop needs a tool entry planner and a plain-text finalizer")
	}
	if config.StepLeaseDuration <= 0 {
		config.StepLeaseDuration = defaultStepLeaseDuration
	}
	if config.MaxStepsPerClaim <= 0 {
		config.MaxStepsPerClaim = RunLoopMaxStepsPerClaim
	}
	if config.MaxClaimDuration <= 0 {
		config.MaxClaimDuration = RunLoopMaxClaimDuration
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(config.Now))
	turns := runtimeactivity.NewAgentTurnService(store, store, models.WithClock(config.Now))
	return &RunLoop{
		store: store, sessions: sessions, activity: activity, turns: turns,
		actions: actions, config: config, logger: logger,
	}, nil
}

func (l *RunLoop) now() time.Time { return l.config.Now().UTC() }

// SetActionCoordinator opens each step's nested-action window on actions.
func (l *RunLoop) SetActionCoordinator(actions *action.ActionCoordinator) { l.actions = actions }

type RunLoopClaim struct {
	Run      *models.AgentRun // claimed, status running, LeaseOwner == WorkerID
	WorkerID string
	Binding  *agentrun.TurnRunnerBinding // resolved by the worker
}

type RunLoopExitReason string

const (
	RunLoopExitFinal     RunLoopExitReason = "final"      // completed (or failed after an explanation)
	RunLoopExitParked    RunLoopExitReason = "parked"     // waiting_* / sleeping
	RunLoopExitYielded   RunLoopExitReason = "yielded"    // fairness, queued
	RunLoopExitRequeued  RunLoopExitReason = "requeued"   // host retry, stale step lease or park re-plans, queued with AvailableAt
	RunLoopExitPaused    RunLoopExitReason = "paused"     // budget or output publication
	RunLoopExitFailed    RunLoopExitReason = "failed"     // Run failed; with Err and the Run left running: an infrastructure error
	RunLoopExitLeaseLost RunLoopExitReason = "lease_lost" // nothing written after the loss
	RunLoopExitEnded     RunLoopExitReason = "ended"      // Run became terminal by command
)

type RunLoopExit struct {
	Run    *models.AgentRun
	Reason RunLoopExitReason
	Steps  int // producer calls in this claim
	Err    error
}

// drive is the state of one claim.
type drive struct {
	l        *RunLoop
	run      *models.AgentRun // latest Run this claim read or wrote
	binding  *agentrun.TurnRunnerBinding
	session  *models.AgentSession // current session (moves on promotion)
	workerID string
	steps    int
}

// Drive runs the claim's loop until it exits. ctx is the claim context the
// worker cancels on Run lease loss.
func (l *RunLoop) Drive(ctx context.Context, claim RunLoopClaim) RunLoopExit {
	if l == nil {
		return RunLoopExit{Run: claim.Run, Reason: RunLoopExitFailed, Err: errors.New("run loop is not configured")}
	}
	if claim.Run == nil || strings.TrimSpace(claim.WorkerID) == "" || claim.Binding == nil || claim.Binding.Producer == nil {
		return RunLoopExit{Run: claim.Run, Reason: RunLoopExitFailed, Err: errors.New("run loop claim needs a Run, a worker and a binding with a producer")}
	}
	if claim.Run.Status != models.AgentRunStatusRunning || claim.Run.LeaseOwner != claim.WorkerID {
		return RunLoopExit{Run: claim.Run, Reason: RunLoopExitLeaseLost, Err: models.ErrLeaseLost}
	}
	d := &drive{l: l, run: models.CloneAgentRun(claim.Run), binding: claim.Binding, session: claim.Binding.Session, workerID: claim.WorkerID}
	started := l.now()
	if err := validatePreparedSkillRuntimes(claim.Binding); err != nil {
		return d.failWithoutStep(ctx, agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: runFailureCodePreparedRuntime,
			Message: "The agent's Skill runtime is not valid, so the run could not start.",
		}, err)
	}
	resumed, exit := d.reconcile(ctx)
	if exit != nil {
		return d.withSteps(*exit)
	}
	rejections := 0
	for {
		if err := ctx.Err(); err != nil {
			return d.withSteps(RunLoopExit{Run: d.run, Reason: RunLoopExitLeaseLost, Err: err})
		}
		if d.steps >= l.config.MaxStepsPerClaim || l.now().Sub(started) >= l.config.MaxClaimDuration {
			return d.withSteps(d.yield(ctx))
		}
		if models.RunAttemptBudgetExceeded(d.run) {
			return d.withSteps(d.pauseAttempts(ctx))
		}
		res := d.runStep(ctx, resumed)
		resumed = nil
		if res.produced {
			d.steps++
		}
		switch res.kind {
		case stepNext:
			rejections = 0
		case stepFinalRejected:
			rejections++
			if rejections >= maximumFinalRejections {
				return d.withSteps(d.failWithoutStep(ctx, agentrun.LoopFailure{
					Kind: conversation.RunFailureKindExecution, Code: runFailureCodeFinalRejected,
					Message: "The agent's final answer was rejected three times in a row, so the run stopped.",
				}, nil))
			}
		default:
			return d.withSteps(res.exit)
		}
	}
}

func (d *drive) withSteps(exit RunLoopExit) RunLoopExit {
	exit.Steps = d.steps
	if exit.Run == nil {
		exit.Run = d.run
	}
	return exit
}

func (d *drive) actor() models.ActivityActor {
	return models.ActivityActor{Type: "worker", ID: d.workerID}
}

// ---------------------------------------------------------------------------
// Run writes

// commitRun writes one Run transition of this claim, fenced by the Run lease.
// When st's step is finished but not yet applied, the same transition applies
// it (applyStep, §6.4): AppliedTurn, the step settlement and the step payload.
// Status "" means running; a running transition after a step that exhausted a
// bounded budget becomes a pause (budget.exhausted).
//
// With cas, a revision conflict or ErrSteeringArrived is returned as is (park
// planning re-plans instead of re-applying). Otherwise the Run is reloaded and
// the request rebuilt, at most maximumRunWriteAttempts times.
func (d *drive) commitRun(ctx context.Context, st *stepState, req runtimeactivity.RunTransitionRequest, cas bool) (*models.AgentRun, error) {
	run := d.run
	var lastErr error
	for attempt := 0; attempt < maximumRunWriteAttempts; attempt++ {
		if attempt > 0 {
			reloaded, err := d.reloadRun(ctx)
			if err != nil {
				return nil, err
			}
			run = reloaded
		}
		if run.Status != models.AgentRunStatusRunning || run.LeaseOwner != d.workerID {
			if models.IsTerminalAgentRunStatus(run.Status) {
				return run, errRunEnded
			}
			return run, models.ErrLeaseLost
		}
		if st != nil && st.finished && !st.applied && st.step.Sequence <= run.LastAppliedTurn {
			st.applied = true
		}
		r := req
		r.ExpectedRevision = run.Revision
		r.LeaseOwner = d.workerID
		if r.Status == "" {
			r.Status = models.AgentRunStatusRunning
		}
		if r.Actor == (models.ActivityActor{}) {
			r.Actor = d.actor()
		}
		r.Payload = maps.Clone(req.Payload)
		if st != nil && st.finished && !st.applied {
			d.fillStepApplication(&r, run, st)
		}
		if r.Summary == "" {
			r.Summary = "Run updated by its agent loop"
		}
		updated, _, err := d.l.activity.TransitionRun(ctx, run.Scope, run.ID, r)
		if err == nil {
			d.run = updated
			if st != nil && st.finished {
				st.applied = true
			}
			return updated, nil
		}
		lastErr = err
		if cas || !errors.Is(err, models.ErrRevisionConflict) {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// errRunEnded is commitRun's error when the Run is already terminal.
var errRunEnded = errors.New("run ended")

// fillStepApplication adds applyStep's fields to a transition (§6.4).
//
// The step row's usage includes the step's compaction calls (phase 4). The
// compactor already charged them to the bounded budget (SettleCompaction), so
// the budget settlement and the exhaustion check use the usage without them,
// while the activity usage carries all of it.
func (d *drive) fillStepApplication(req *runtimeactivity.RunTransitionRequest, run *models.AgentRun, st *stepState) {
	step := st.step
	budgetStep := step
	if st.compaction != (models.TurnUsage{}) {
		budgetStep = models.CloneAgentTurn(step)
		budgetStep.Usage = subtractCompactionUsage(step.Usage, st.compaction)
	}
	req.AppliedTurn = step.Sequence
	req.TurnID = step.ID
	req.CausationID = step.ID
	if req.Payload == nil {
		req.Payload = map[string]interface{}{}
	}
	req.Payload["turnSequence"] = step.Sequence
	req.Payload["usage"] = step.Usage
	req.Payload["toolCallCount"] = step.ToolCallCount
	if step.StopReason != "" {
		req.Payload["stopReason"] = step.StopReason
	}
	req.Payload["skillSelections"] = append([]models.HostedSkillSelection(nil), step.SkillSelections...)
	if id := strings.TrimSpace(step.DefinitionID); id != "" {
		req.Payload["definitionId"] = id
	}
	if version := strings.TrimSpace(step.DefinitionVersion); version != "" {
		req.Payload["definitionVersion"] = version
	}
	if step.Status == models.AgentTurnStatusFailed && strings.TrimSpace(step.Error) != "" {
		req.Payload["stepError"] = conversation.BoundedTurnFailureText(step.Error)
	}
	if st.kernelState != nil {
		req.Checkpoint = st.kernelState
		if current := runbookCurrentStep(st.kernelState); current != "" {
			req.Payload["runbookStep"] = current
		}
		if delta := routine.RunbookTraceDelta(run.Checkpoint, st.kernelState); len(delta) > 0 {
			req.Payload["runbookTraceDelta"] = delta
		}
	}
	if req.Summary == "" {
		if step.OutputSummary != "" && step.Status == models.AgentTurnStatusCompleted {
			req.Summary = step.OutputSummary
		} else {
			req.Summary = fmt.Sprintf("Applied step %d", step.Sequence)
		}
	}
	if req.Status == models.AgentRunStatusRunning && stepExhaustsBudget(run, budgetStep, st.produced) {
		req.Status = models.AgentRunStatusPaused
		req.Summary = stepBudgetExhaustedAfterStepSummary
	}
	fillStepBudgetSettlement(req, run, budgetStep, st.produced)
	if st.compaction != (models.TurnUsage{}) {
		if activity := budgetUsageForStep(step.Usage, st.produced); activity != (models.BudgetUsage{}) {
			req.ActivityUsageDelta = &activity
		}
	}
}

// subtractCompactionUsage removes a step's compaction usage from its row
// usage (never below zero).
func subtractCompactionUsage(usage, compaction models.TurnUsage) models.TurnUsage {
	usage.InputTokens = max(usage.InputTokens-compaction.InputTokens, 0)
	usage.OutputTokens = max(usage.OutputTokens-compaction.OutputTokens, 0)
	usage.Cost = max(usage.Cost-compaction.Cost, 0)
	return usage
}

// runbookCurrentStep is the current runbook step named by a deterministic
// producer's kernel state.
func runbookCurrentStep(state map[string]interface{}) string {
	runbookState, ok := state["runbook"].(map[string]interface{})
	if !ok {
		return ""
	}
	current, _ := runbookState["current"].(string)
	return strings.TrimSpace(current)
}

func (d *drive) reloadRun(ctx context.Context) (*models.AgentRun, error) {
	run, err := d.l.store.GetAgentRun(ctx, d.run.Scope, d.run.ID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, models.ErrRunNotFound
	}
	d.run = run
	return run, nil
}

// ---------------------------------------------------------------------------
// Exits

// statusExit maps a Run that stopped running after a loop write to its exit.
func (d *drive) statusExit(run *models.AgentRun) *RunLoopExit {
	exit := RunLoopExit{Run: run}
	switch {
	case run.Status == models.AgentRunStatusPaused:
		exit.Reason = RunLoopExitPaused
	case run.Status == models.AgentRunStatusQueued:
		exit.Reason = RunLoopExitRequeued
	case models.IsWaitingRunStatus(run.Status):
		exit.Reason = RunLoopExitParked
	case run.Status == models.AgentRunStatusCompleted:
		exit.Reason = RunLoopExitFinal
	case run.Status == models.AgentRunStatusFailed:
		exit.Reason = RunLoopExitFailed
	case models.IsTerminalAgentRunStatus(run.Status):
		exit.Reason = RunLoopExitEnded
	default:
		exit.Reason = RunLoopExitLeaseLost
	}
	return &exit
}

// errorExit leaves the Run as it is (its lease expires and a later claim
// resumes from the log).
func (d *drive) errorExit(err error) *RunLoopExit {
	return &RunLoopExit{Run: d.run, Reason: RunLoopExitFailed, Err: err}
}

// writeErrorExit classifies an error of a fenced write.
func (d *drive) writeErrorExit(err error) *RunLoopExit {
	switch {
	case errors.Is(err, errRunEnded):
		return &RunLoopExit{Run: d.run, Reason: RunLoopExitEnded}
	case isLeaseLoss(err):
		return &RunLoopExit{Run: d.run, Reason: RunLoopExitLeaseLost, Err: err}
	}
	return d.errorExit(err)
}

func isLeaseLoss(err error) bool {
	return errors.Is(err, models.ErrLeaseLost) || errors.Is(err, models.ErrTurnLeaseHeld) || errors.Is(err, context.Canceled)
}

// yield ends the claim for fairness (D13); the next claim resumes from the log.
func (d *drive) yield(ctx context.Context) RunLoopExit {
	run, err := d.commitRun(ctx, nil, runtimeactivity.RunTransitionRequest{
		Status: models.AgentRunStatusQueued, EventType: "run.yielded", Summary: "Run yielded after its step slice",
		Payload: map[string]interface{}{"steps": d.steps},
	}, false)
	if err != nil {
		return *d.writeErrorExit(err)
	}
	return RunLoopExit{Run: run, Reason: RunLoopExitYielded}
}

// requeue puts the Run back in the queue, claimable at availableAt. st's
// finished step, when not yet applied, is applied by the same write.
func (d *drive) requeue(ctx context.Context, st *stepState, availableAt *time.Time, reason, summary string) RunLoopExit {
	run, err := d.commitRun(ctx, st, runtimeactivity.RunTransitionRequest{
		Status: models.AgentRunStatusQueued, EventType: "run.requeued", Summary: summary, AvailableAt: availableAt,
		Payload: map[string]interface{}{"reason": reason},
	}, false)
	if err != nil {
		return *d.writeErrorExit(err)
	}
	return RunLoopExit{Run: run, Reason: RunLoopExitRequeued}
}

// pauseAttempts pauses a Run that used up its attempt budget.
func (d *drive) pauseAttempts(ctx context.Context) RunLoopExit {
	run, err := d.commitRun(ctx, nil, runtimeactivity.RunTransitionRequest{
		Status: models.AgentRunStatusPaused, EventType: "budget.exhausted",
		Summary: "Run paused before exceeding its autonomous attempt budget",
		Payload: map[string]interface{}{
			"budgetState": models.BudgetStateExhausted, "dimension": "attempts",
			"used": d.run.BudgetUsage.Attempts, "limit": d.run.Budget.MaxAttempts,
		},
	}, false)
	if err != nil {
		return *d.writeErrorExit(err)
	}
	return RunLoopExit{Run: run, Reason: RunLoopExitPaused}
}

// failWithoutStep fails the Run outside a running step (before the first
// step, or after the step that triggered it was applied). It is never
// explained by the model.
func (d *drive) failWithoutStep(ctx context.Context, failure agentrun.LoopFailure, cause error) RunLoopExit {
	failure.Explainable = false
	res := d.fail(ctx, nil, failure, cause, "")
	if res.kind != stepExit {
		return RunLoopExit{Run: d.run, Reason: RunLoopExitFailed, Err: errors.New("run failure did not end the run")}
	}
	return res.exit
}

func validatePreparedSkillRuntimes(binding *agentrun.TurnRunnerBinding) error {
	seen := make(map[string]bool, len(binding.PreparedRuntimes))
	for index := range binding.PreparedRuntimes {
		prepared := &binding.PreparedRuntimes[index]
		if strings.TrimSpace(prepared.DeploymentID) == "" || strings.TrimSpace(prepared.BindingID) == "" || prepared.BindingRevision < 1 || strings.TrimSpace(prepared.SkillID) == "" || strings.TrimSpace(prepared.SkillVersion) == "" {
			return errors.New("prepared Skill runtime binding identity is invalid")
		}
		if err := skill.ValidatePreparedRuntimeReference(&prepared.Runtime); err != nil {
			return fmt.Errorf("prepared Skill runtime binding is invalid: %w", err)
		}
		key := fmt.Sprintf("%s:%s@%d:%s@%s", prepared.DeploymentID, prepared.BindingID, prepared.BindingRevision, prepared.SkillID, prepared.SkillVersion)
		if seen[key] {
			return errors.New("prepared Skill runtime binding is duplicated")
		}
		seen[key] = true
		authorized := false
		for actionIndex := range binding.ModelActions {
			action := &binding.ModelActions[actionIndex]
			if action.DeploymentID == prepared.DeploymentID && action.BindingID == prepared.BindingID && action.BindingRevision == prepared.BindingRevision && action.SkillID == prepared.SkillID && action.Version == prepared.SkillVersion {
				authorized = true
				break
			}
		}
		if !authorized {
			return errors.New("prepared Skill runtime is not bound to an authorized model action")
		}
	}
	return nil
}
