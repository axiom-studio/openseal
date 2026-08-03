package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

type TurnExecutionContext struct {
	Run  *AgentRun
	Turn *AgentTurn
}

// TurnRunner performs one bounded, proposal-only reasoning step. External side
// effects are dispatched later through durable governed skill calls.
type TurnRunner interface {
	RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error)
}

// TurnBudgetPlanner deterministically describes the maximum capacity a runner
// can consume before any external provider or capability is invoked.
type TurnBudgetPlanner interface {
	PlanTurnBudget(context.Context, TurnExecutionContext) (BudgetUsage, error)
}

type TurnRunnerFunc func(context.Context, TurnExecutionContext) (*TurnOutcome, error)

func (f TurnRunnerFunc) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	return f(ctx, input)
}

type TurnOutcome struct {
	ModelProvider          string
	Model                  string
	SkillSelections        []HostedSkillSelection
	Decisions              []TurnDecision
	ProposedActions        []TurnAction
	ProposedFork           *TurnForkProposal
	ProposedDelegation     *TurnDelegationProposal
	ProposedRunbook        *TurnRunbookProposal
	OutputSummary          string
	Usage                  TurnUsage
	ContinuationCheckpoint map[string]interface{}
	NextRunStatus          AgentRunStatus
	WakeCondition          *WakeCondition
	RunOutput              map[string]interface{}
	RunError               string
	EvidenceClaims         []EvidenceClaim
	EvidenceGrounding      *EvidenceGroundingReview
}

type AdvanceAgentRunRequest struct {
	Scope             Scope
	RunID             string
	WorkerID          string
	LeaseDuration     time.Duration
	DefinitionID      string
	DefinitionVersion string
	ModelProvider     string
	Model             string
	InputContextRefs  []string
	PlanRevision      int64
	BudgetReservation BudgetUsage
}

type AdvanceAgentRunResult struct {
	Run        *AgentRun
	Turn       *AgentTurn
	Event      *ActivityEvent
	Reconciled bool
}

type TurnCoordinator struct {
	portfolio          PortfolioStore
	activity           *RunActivityService
	turns              *AgentTurnService
	afterTurnPersisted func() error
}

func NewTurnCoordinator(portfolio PortfolioStore, activity RunActivityStore, turns AgentTurnStore) *TurnCoordinator {
	return &TurnCoordinator{
		portfolio: portfolio, activity: NewRunActivityService(portfolio, activity),
		turns: NewAgentTurnService(portfolio, turns),
	}
}

func (c *TurnCoordinator) Advance(ctx context.Context, req AdvanceAgentRunRequest, runner TurnRunner) (*AdvanceAgentRunResult, error) {
	if c == nil || c.portfolio == nil || c.activity == nil || c.turns == nil {
		return nil, errors.New("turn coordinator is not configured")
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		return nil, errors.New("worker id is required")
	}
	run, err := c.portfolio.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	if run.Status == AgentRunStatusQueued || run.Status == AgentRunStatusPlanning {
		run, _, err = c.activity.TransitionRun(ctx, req.Scope, req.RunID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Actor: ActivityActor{Type: "worker", ID: req.WorkerID},
			Summary: "Run started for a bounded agent turn",
		})
		if err != nil {
			return nil, err
		}
	}
	if run.Status != AgentRunStatusRunning {
		return nil, fmt.Errorf("run %s cannot advance from %s", run.ID, run.Status)
	}

	pending, err := c.turns.ListTurns(ctx, AgentTurnFilter{
		Scope: req.Scope, RunID: req.RunID, AfterSequence: run.LastAppliedTurn, Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 && terminalAgentTurnStatus(pending[0].Status) {
		return c.applyFinishedTurn(ctx, run, pending[0], req.WorkerID, true)
	}
	if runAttemptBudgetExceeded(run) {
		paused, event, pauseErr := c.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusPaused, LeaseOwner: req.WorkerID,
			Summary: "Run paused before exceeding its autonomous attempt budget", EventType: "budget.exhausted",
			Actor: ActivityActor{Type: "worker", ID: req.WorkerID},
			Payload: map[string]interface{}{
				"dimension": "attempts", "used": run.BudgetUsage.Attempts, "limit": run.Budget.MaxAttempts,
			},
		})
		if pauseErr != nil {
			return nil, pauseErr
		}
		return &AdvanceAgentRunResult{Run: paused, Event: event}, ErrBudgetExhausted
	}
	if runner == nil {
		return nil, errors.New("turn runner is required")
	}

	var turn *AgentTurn
	if len(pending) > 0 {
		turn, err = c.turns.ClaimTurn(ctx, req.Scope, pending[0].ID, req.WorkerID, req.LeaseDuration)
	} else {
		turn, err = c.turns.BeginTurn(ctx, BeginAgentTurnRequest{
			Scope: req.Scope, RunID: req.RunID, WorkerID: req.WorkerID, LeaseDuration: req.LeaseDuration,
			DefinitionID: req.DefinitionID, DefinitionVersion: req.DefinitionVersion,
			ModelProvider: req.ModelProvider, Model: req.Model, InputContextRefs: req.InputContextRefs,
			PlanRevision: req.PlanRevision,
		})
	}
	if err != nil {
		return nil, err
	}
	if run.Budget != nil {
		existing, reserved := run.BudgetReservations[turn.ID]
		reservationUsage := req.BudgetReservation
		if planner, ok := runner.(TurnBudgetPlanner); ok {
			planned, planErr := planner.PlanTurnBudget(ctx, TurnExecutionContext{Run: cloneAgentRun(run), Turn: cloneAgentTurn(turn)})
			if planErr != nil {
				if errors.Is(planErr, ErrBudgetExhausted) {
					var admissionErr *BudgetAdmissionError
					if errors.As(planErr, &admissionErr) {
						return c.pauseBeforeTurnBudget(ctx, run, turn, req.WorkerID, planErr.Error(), admissionErr.Admission)
					}
					return c.pauseBeforeTurnBudget(ctx, run, turn, req.WorkerID, planErr.Error(), BudgetAdmission{Reason: BudgetAdmissionHostedInput, Dimension: "unknown"})
				}
				return nil, planErr
			}
			reservationUsage, err = reservationUsage.Add(planned)
			if err != nil {
				return nil, err
			}
		}
		reservationUsage.Turns = 1
		if err := reservationUsage.Validate(); err != nil {
			return nil, err
		}
		if !reserved || existing.Usage != reservationUsage {
			leaseOwner := ""
			if run.LeaseOwner != "" {
				leaseOwner = req.WorkerID
			}
			otherReservations := make(map[string]BudgetReservation, len(run.BudgetReservations))
			for id, reservation := range run.BudgetReservations {
				if id != turn.ID {
					otherReservations[id] = reservation
				}
			}
			effective, err := EffectiveBudgetUsage(run.BudgetUsage, otherReservations)
			if err != nil {
				return nil, err
			}
			projected, err := effective.Add(reservationUsage)
			if err != nil {
				return nil, err
			}
			exceeded, _, err := BudgetWouldExceed(*run.Budget, projected)
			if err != nil {
				return nil, err
			}
			if exceeded {
				admission := budgetReservationAdmission(*run.Budget, effective, reservationUsage)
				return c.pauseBeforeTurnBudget(ctx, run, turn, req.WorkerID, "Run paused before exceeding its autonomous budget", admission)
			}
			reservation := &BudgetReservation{ID: turn.ID, Usage: reservationUsage, CreatedAt: c.activity.now()}
			transition := RunTransitionRequest{
				ExpectedRevision: run.Revision, Status: AgentRunStatusRunning,
				Summary: "Reserved capacity for a bounded agent turn", EventType: "budget.reserved",
				Actor: ActivityActor{Type: "worker", ID: req.WorkerID}, LeaseOwner: leaseOwner,
				BudgetReservation: reservation,
			}
			if reserved {
				reservation.CreatedAt = existing.CreatedAt
				transition.Summary = "Reconciled capacity for a retrying bounded agent turn"
				transition.EventType = "budget.reservation_reconciled"
				transition.BudgetReservation = nil
				transition.ReplaceBudgetReservation = reservation
			}
			reservedRun, _, err := c.activity.TransitionRun(ctx, run.Scope, run.ID, transition)
			if err != nil {
				return nil, err
			}
			run = reservedRun
		}
	}
	executionCtx, cancelExecution := context.WithCancel(ctx)
	durationDeadline := false
	durationDeadlineChargeMS := int64(0)
	if run.Budget != nil && run.Budget.MaxDurationMS > 0 {
		remainingMS := run.Budget.MaxDurationMS - run.BudgetUsage.DurationMS
		for id, reservation := range run.BudgetReservations {
			if id != turn.ID {
				remainingMS -= reservation.Usage.DurationMS
			}
		}
		if remainingMS <= 0 {
			cancelExecution()
			finished, finishErr := c.turns.FinishTurn(ctx, req.Scope, turn.ID, FinishAgentTurnRequest{
				ExpectedRevision: turn.Revision, Status: AgentTurnStatusCanceled, WorkerID: req.WorkerID,
				NextRunStatus: AgentRunStatusPaused, OutputSummary: "Run paused before exceeding its autonomous duration budget",
			})
			if finishErr != nil {
				return nil, finishErr
			}
			result, applyErr := c.applyFinishedTurn(ctx, run, finished, req.WorkerID, false)
			if applyErr != nil {
				return nil, applyErr
			}
			return result, ErrBudgetExhausted
		}
		cancelExecution()
		executionCtx, cancelExecution = context.WithTimeout(ctx, time.Duration(remainingMS)*time.Millisecond)
		durationDeadline = true
		durationDeadlineChargeMS = remainingMS
	}
	heartbeatDone := make(chan turnLeaseHeartbeatResult, 1)
	go c.heartbeatTurnLease(executionCtx, cancelExecution, req.Scope, turn, req.WorkerID, req.LeaseDuration, heartbeatDone)
	executionStarted := time.Now()
	outcome, runErr := runner.RunTurn(executionCtx, TurnExecutionContext{Run: cloneAgentRun(run), Turn: cloneAgentTurn(turn)})
	executionDurationMS := time.Since(executionStarted).Milliseconds()
	durationExpired := durationDeadline && errors.Is(executionCtx.Err(), context.DeadlineExceeded)
	if durationExpired && executionDurationMS < durationDeadlineChargeMS {
		executionDurationMS = durationDeadlineChargeMS
	}
	cancelExecution()
	heartbeat := <-heartbeatDone
	if heartbeat.turn != nil {
		turn = heartbeat.turn
	}
	if heartbeat.err != nil {
		if runErr == nil || errors.Is(heartbeat.err, ErrTurnLeaseHeld) || errors.Is(heartbeat.err, ErrLeaseLost) {
			runErr = heartbeat.err
		}
	}
	refreshed, refreshErr := c.portfolio.GetAgentRun(ctx, req.Scope, req.RunID)
	if refreshErr != nil {
		return nil, refreshErr
	}
	if refreshed.Status != AgentRunStatusRunning || refreshed.LeaseOwner != "" && refreshed.LeaseOwner != req.WorkerID {
		released, releaseErr := c.turns.ReleaseTurn(ctx, req.Scope, turn.ID, turn.Revision, req.WorkerID)
		if releaseErr != nil && !errors.Is(releaseErr, ErrLeaseLost) {
			return nil, releaseErr
		}
		return &AdvanceAgentRunResult{Run: refreshed, Turn: released}, ErrLeaseLost
	}
	run = refreshed
	if heartbeat.err != nil {
		released, releaseErr := c.turns.ReleaseTurn(ctx, req.Scope, turn.ID, turn.Revision, req.WorkerID)
		if releaseErr != nil && !errors.Is(releaseErr, ErrLeaseLost) && !errors.Is(releaseErr, ErrTurnLeaseHeld) {
			return nil, releaseErr
		}
		return &AdvanceAgentRunResult{Run: run, Turn: released}, heartbeat.err
	}
	if errors.Is(runErr, ErrTurnHostUnavailable) && !durationExpired {
		released, releaseErr := c.turns.ReleaseTurn(ctx, req.Scope, turn.ID, turn.Revision, req.WorkerID)
		if releaseErr != nil {
			return nil, releaseErr
		}
		retryAttempt := released.Revision / 2
		if retryAttempt < 1 {
			retryAttempt = 1
		}
		retryAt := c.activity.now().Add(hostedTurnRetryDelay(retryAttempt))
		leaseOwner := ""
		if run.LeaseOwner != "" {
			leaseOwner = req.WorkerID
		}
		requeued, event, transitionErr := c.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusSleeping, LeaseOwner: leaseOwner,
			WakeCondition: &WakeCondition{Type: "timer", WakeAt: &retryAt, Reference: "hosted-turn-retry"},
			Summary:       "Agent turn host unavailable; the bounded turn will retry", EventType: "turn.retry_scheduled",
			Actor:       ActivityActor{Type: "worker", ID: req.WorkerID},
			TurnID:      turn.ID,
			CausationID: turn.ID,
			Payload: map[string]interface{}{
				"attempt": retryAttempt,
				"retryAt": retryAt.UTC().Format(time.RFC3339Nano),
			},
		})
		if transitionErr != nil {
			return nil, transitionErr
		}
		return &AdvanceAgentRunResult{Run: requeued, Turn: released, Event: event}, runErr
	}
	executionErr := runErr
	finish := FinishAgentTurnRequest{ExpectedRevision: turn.Revision, WorkerID: req.WorkerID}
	if durationExpired {
		executionErr = ErrBudgetExhausted
		finish.Status = AgentTurnStatusCanceled
		finish.NextRunStatus = AgentRunStatusPaused
		finish.OutputSummary = "Run paused after reaching its autonomous duration budget"
		finish.Usage.DurationMS = executionDurationMS
	} else if runErr != nil {
		finish.Status = AgentTurnStatusFailed
		finish.Error = runErr.Error()
		finish.NextRunStatus = AgentRunStatusFailed
		finish.RunError = runErr.Error()
		finish.OutputSummary = "Bounded agent turn failed"
		finish.Usage.DurationMS = executionDurationMS
	} else if outcome == nil {
		executionErr = errors.New("turn runner returned no outcome")
		finish.Status = AgentTurnStatusFailed
		finish.Error = executionErr.Error()
		finish.NextRunStatus = AgentRunStatusFailed
		finish.RunError = finish.Error
		finish.OutputSummary = "Bounded agent turn failed"
	} else {
		if outcome.Usage.DurationMS < executionDurationMS {
			outcome.Usage.DurationMS = executionDurationMS
		}
		if err := validateTurnOutcome(run.Status, outcome); err != nil {
			executionErr = err
			finish.Status = AgentTurnStatusFailed
			finish.Error = err.Error()
			finish.NextRunStatus = AgentRunStatusFailed
			finish.RunError = err.Error()
			finish.OutputSummary = "Bounded agent turn produced an invalid outcome"
		} else {
			finish.Status = AgentTurnStatusCompleted
			finish.ModelProvider = outcome.ModelProvider
			finish.Model = outcome.Model
			finish.SkillSelections = outcome.SkillSelections
			finish.Decisions = outcome.Decisions
			finish.RequestedActions = outcome.ProposedActions
			finish.RequestedFork = outcome.ProposedFork
			finish.RequestedDelegation = outcome.ProposedDelegation
			finish.RequestedRunbook = outcome.ProposedRunbook
			finish.OutputSummary = outcome.OutputSummary
			finish.Usage = outcome.Usage
			finish.ContinuationCheckpoint = outcome.ContinuationCheckpoint
			finish.NextRunStatus = outcome.NextRunStatus
			finish.WakeCondition = outcome.WakeCondition
			finish.RunOutput = outcome.RunOutput
			finish.RunError = outcome.RunError
			finish.EvidenceClaims = outcome.EvidenceClaims
			finish.EvidenceGrounding = outcome.EvidenceGrounding
		}
	}
	if run.Budget != nil && finish.Status == AgentTurnStatusCompleted {
		delta := budgetUsageForTurn(finish.Usage)
		usage, usageErr := run.BudgetUsage.Add(delta)
		if usageErr != nil {
			return nil, usageErr
		}
		state, _, usageErr := EvaluateBudget(*run.Budget, usage)
		if usageErr != nil {
			return nil, usageErr
		}
		if state == BudgetStateExhausted && !isTerminalAgentRunStatus(finish.NextRunStatus) && len(finish.RequestedActions) == 0 {
			finish.NextRunStatus = AgentRunStatusPaused
			finish.OutputSummary = "Run paused after reaching its autonomous budget"
		}
	}
	if finish.Status == AgentTurnStatusCompleted {
		finish.ContinuationCheckpoint = checkpointTurnContinuity(finish.ContinuationCheckpoint, &AgentTurn{
			Sequence: turn.Sequence, Status: finish.Status, OutputSummary: finish.OutputSummary,
			RequestedActions: finish.RequestedActions, RequestedFork: finish.RequestedFork,
			RequestedDelegation: finish.RequestedDelegation, RequestedRunbook: finish.RequestedRunbook,
			NextRunStatus: finish.NextRunStatus,
		})
	}
	turn, err = c.turns.FinishTurn(ctx, req.Scope, turn.ID, finish)
	if err != nil {
		return nil, err
	}
	if c.afterTurnPersisted != nil {
		if err := c.afterTurnPersisted(); err != nil {
			return &AdvanceAgentRunResult{Run: run, Turn: turn}, err
		}
	}
	result, err := c.applyFinishedTurn(ctx, run, turn, req.WorkerID, false)
	if err != nil {
		return nil, err
	}
	return result, executionErr
}

func (c *TurnCoordinator) pauseBeforeTurnBudget(ctx context.Context, run *AgentRun, turn *AgentTurn, workerID, summary string, admission BudgetAdmission) (*AdvanceAgentRunResult, error) {
	if summary == "" {
		summary = "Run paused before exceeding its autonomous budget"
	}
	admission.TurnID = turn.ID
	admission.EvaluatedAt = c.activity.now()
	finished, err := c.turns.FinishTurn(ctx, run.Scope, turn.ID, FinishAgentTurnRequest{
		ExpectedRevision: turn.Revision, Status: AgentTurnStatusCanceled, WorkerID: workerID,
		NextRunStatus: AgentRunStatusPaused, OutputSummary: summary, BudgetAdmission: &admission,
	})
	if err != nil {
		return nil, err
	}
	result, err := c.applyFinishedTurn(ctx, run, finished, workerID, false)
	if err != nil {
		return nil, err
	}
	return result, ErrBudgetExhausted
}

type turnLeaseHeartbeatResult struct {
	turn *AgentTurn
	err  error
}

func (c *TurnCoordinator) heartbeatTurnLease(ctx context.Context, cancel context.CancelFunc, scope Scope, turn *AgentTurn, workerID string, leaseDuration time.Duration, done chan<- turnLeaseHeartbeatResult) {
	if leaseDuration <= 0 {
		leaseDuration = 5 * time.Minute
	}
	interval := leaseDuration / 3
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	latest := turn
	for {
		select {
		case <-ctx.Done():
			done <- turnLeaseHeartbeatResult{turn: latest}
			return
		case <-ticker.C:
			renewed, err := c.turns.RenewTurn(ctx, scope, turn.ID, workerID, leaseDuration)
			if err != nil {
				if ctx.Err() != nil {
					done <- turnLeaseHeartbeatResult{turn: latest}
					return
				}
				cancel()
				done <- turnLeaseHeartbeatResult{turn: latest, err: err}
				return
			}
			latest = renewed
		}
	}
}

func hostedTurnRetryDelay(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 4 {
		attempt = 4
	}
	return 5 * time.Second * time.Duration(1<<(attempt-1))
}

func (c *TurnCoordinator) applyFinishedTurn(ctx context.Context, run *AgentRun, turn *AgentTurn, workerID string, reconciled bool) (*AdvanceAgentRunResult, error) {
	if turn.Sequence <= run.LastAppliedTurn {
		return &AdvanceAgentRunResult{Run: run, Turn: turn, Reconciled: reconciled}, nil
	}
	if turn.Sequence != run.LastAppliedTurn+1 {
		return nil, fmt.Errorf("%w: turn %d cannot follow applied turn %d", ErrRevisionConflict, turn.Sequence, run.LastAppliedTurn)
	}
	summary := turn.OutputSummary
	if summary == "" {
		summary = fmt.Sprintf("Applied bounded agent turn %d", turn.Sequence)
	}
	leaseOwner := ""
	if run.LeaseOwner != "" {
		if run.LeaseOwner != workerID {
			return nil, ErrLeaseLost
		}
		leaseOwner = workerID
	}
	turnUsageDelta := budgetUsageForTurn(turn.Usage)
	if turn.Status == AgentTurnStatusCanceled && turn.Usage == (TurnUsage{}) {
		turnUsageDelta.Turns = 0
	}
	var activityUsageDelta *BudgetUsage
	if turnUsageDelta != (BudgetUsage{}) {
		activityUsageDelta = &turnUsageDelta
	}
	var budgetDelta *BudgetUsage
	if run.Budget != nil {
		if _, reserved := run.BudgetReservations[turn.ID]; reserved {
			budgetDelta = &turnUsageDelta
		}
	}
	activityPayload := map[string]interface{}{
		"turnSequence":    turn.Sequence,
		"skillSelections": append([]HostedSkillSelection(nil), turn.SkillSelections...),
		"decisions":       append([]TurnDecision(nil), turn.Decisions...),
		"usage":           turn.Usage,
	}
	if definitionID := strings.TrimSpace(turn.DefinitionID); definitionID != "" {
		activityPayload["definitionId"] = definitionID
	}
	if definitionVersion := strings.TrimSpace(turn.DefinitionVersion); definitionVersion != "" {
		activityPayload["definitionVersion"] = definitionVersion
	}
	if step := runbookStepFromCheckpoint(turn.ContinuationCheckpoint); step != "" {
		activityPayload["runbookStep"] = step
	}
	if traceDelta := runbookTraceDelta(run.Checkpoint, turn.ContinuationCheckpoint); len(traceDelta) > 0 {
		activityPayload["runbookTraceDelta"] = traceDelta
	}
	if len(turn.RequestedActions) > 0 {
		activityPayload["requestedActions"] = append([]TurnAction(nil), turn.RequestedActions...)
	}
	if turn.RequestedFork != nil {
		activityPayload["requestedFork"] = turn.RequestedFork
	}
	if turn.RequestedDelegation != nil {
		activityPayload["requestedDelegation"] = turn.RequestedDelegation
	}
	if turn.RequestedRunbook != nil {
		activityPayload["requestedRunbook"] = turn.RequestedRunbook
	}
	eventType := ""
	if turn.BudgetAdmission != nil {
		eventType = "budget.exhausted"
		activityPayload["budgetState"] = BudgetStateExhausted
		activityPayload["admission"] = *turn.BudgetAdmission
	} else if turn.NextRunStatus == AgentRunStatusPaused && turnExhaustsBudget(run, turn) {
		eventType = "budget.exhausted"
		activityPayload["budgetState"] = BudgetStateExhausted
	}
	updated, event, err := c.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: turn.NextRunStatus, Summary: summary,
		EventType: eventType,
		Actor:     ActivityActor{Type: "worker", ID: workerID}, Checkpoint: turn.ContinuationCheckpoint,
		WakeCondition: turn.WakeCondition, Output: turn.RunOutput, Error: turn.RunError,
		TurnID: turn.ID, AppliedTurn: turn.Sequence, CausationID: turn.ID, Payload: activityPayload,
		LeaseOwner:                leaseOwner,
		BudgetAdmission:           turn.BudgetAdmission,
		BudgetUsageDelta:          budgetDelta,
		ActivityUsageDelta:        activityUsageDelta,
		SettleBudgetReservationID: budgetReservationID(run, turn),
	})
	if err != nil {
		return nil, err
	}
	return &AdvanceAgentRunResult{Run: updated, Turn: turn, Event: event, Reconciled: reconciled}, nil
}

func runbookStepFromCheckpoint(checkpoint map[string]interface{}) string {
	runbookState, ok := checkpoint["runbook"].(map[string]interface{})
	if !ok {
		return ""
	}
	current, _ := runbookState["current"].(string)
	return strings.TrimSpace(current)
}

func turnExhaustsBudget(run *AgentRun, turn *AgentTurn) bool {
	if run == nil || turn == nil || run.Budget == nil {
		return false
	}
	usage, err := run.BudgetUsage.Add(budgetUsageForTurn(turn.Usage))
	if err != nil {
		return false
	}
	reservations := make(map[string]BudgetReservation, len(run.BudgetReservations))
	for id, reservation := range run.BudgetReservations {
		if id != turn.ID {
			reservations[id] = reservation
		}
	}
	effective, err := EffectiveBudgetUsage(usage, reservations)
	if err != nil {
		return false
	}
	state, _, err := EvaluateBudget(*run.Budget, effective)
	return err == nil && state == BudgetStateExhausted
}

func budgetReservationID(run *AgentRun, turn *AgentTurn) string {
	if run == nil || turn == nil || run.Budget == nil {
		return ""
	}
	if _, reserved := run.BudgetReservations[turn.ID]; !reserved {
		return ""
	}
	return turn.ID
}

func budgetUsageForTurn(usage TurnUsage) BudgetUsage {
	costMicros := int64(0)
	if usage.Cost > 0 {
		costMicros = int64(math.Round(usage.Cost * 1_000_000))
	}
	return BudgetUsage{Turns: 1, InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens), CostMicros: costMicros, DurationMS: usage.DurationMS}
}

func validateTurnOutcome(current AgentRunStatus, outcome *TurnOutcome) error {
	if err := outcome.Usage.Validate(); err != nil {
		return err
	}
	if outcome.NextRunStatus == "" {
		outcome.NextRunStatus = AgentRunStatusRunning
	}
	if !canTransitionAgentRun(current, outcome.NextRunStatus) {
		return fmt.Errorf("invalid next run status %s", outcome.NextRunStatus)
	}
	if isWaitingRunStatus(outcome.NextRunStatus) && outcome.WakeCondition == nil {
		return fmt.Errorf("next run status %s requires a wake condition", outcome.NextRunStatus)
	}
	proposalCount := 0
	if len(outcome.ProposedActions) > 0 {
		proposalCount++
		if len(outcome.ProposedActions) != 1 {
			return errors.New("a bounded Turn can propose exactly one action at a time")
		}
		if err := uniqueIDs(outcome.ProposedActions[0].EvidenceRefs, "turn action evidence"); err != nil {
			return err
		}
	}
	if outcome.ProposedFork != nil {
		proposalCount++
	}
	if outcome.ProposedDelegation != nil {
		proposalCount++
	}
	if outcome.ProposedRunbook != nil {
		proposalCount++
	}
	if proposalCount > 1 {
		return errors.New("a bounded Turn can propose only one action, fork, delegation, or runbook")
	}
	if outcome.ProposedDelegation != nil {
		if err := outcome.ProposedDelegation.Validate(); err != nil {
			return err
		}
		if outcome.NextRunStatus != AgentRunStatusRunning {
			return errors.New("a proposed delegation must leave the source Run running until materialized")
		}
	}
	if outcome.ProposedFork != nil {
		if err := outcome.ProposedFork.Validate(); err != nil {
			return err
		}
		if outcome.NextRunStatus != AgentRunStatusRunning {
			return errors.New("a proposed fork must leave the source Run running until materialized")
		}
	}
	if outcome.ProposedRunbook != nil {
		if err := outcome.ProposedRunbook.Validate(); err != nil {
			return err
		}
		if outcome.NextRunStatus != AgentRunStatusRunning {
			return errors.New("a proposed runbook must leave the source Run running until materialized")
		}
	}
	return nil
}
