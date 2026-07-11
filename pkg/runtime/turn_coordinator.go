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

type TurnRunnerFunc func(context.Context, TurnExecutionContext) (*TurnOutcome, error)

func (f TurnRunnerFunc) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	return f(ctx, input)
}

type TurnOutcome struct {
	Decisions              []TurnDecision
	ProposedActions        []TurnAction
	OutputSummary          string
	Usage                  TurnUsage
	ContinuationCheckpoint map[string]interface{}
	NextRunStatus          AgentRunStatus
	WakeCondition          *WakeCondition
	RunOutput              map[string]interface{}
	RunError               string
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
	if runner == nil {
		return nil, errors.New("turn runner is required")
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
		if _, reserved := run.BudgetReservations[turn.ID]; !reserved {
			leaseOwner := ""
			if run.LeaseOwner != "" {
				leaseOwner = req.WorkerID
			}
			reservationUsage := req.BudgetReservation
			reservationUsage.Turns = 1
			if err := reservationUsage.Validate(); err != nil {
				return nil, err
			}
			effective, err := EffectiveBudgetUsage(run.BudgetUsage, run.BudgetReservations)
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
				turn, finishErr := c.turns.FinishTurn(ctx, req.Scope, turn.ID, FinishAgentTurnRequest{
					ExpectedRevision: turn.Revision, Status: AgentTurnStatusCanceled, WorkerID: req.WorkerID,
					NextRunStatus: AgentRunStatusPaused, OutputSummary: "Run paused before exceeding its autonomous budget",
				})
				if finishErr != nil {
					return nil, finishErr
				}
				result, applyErr := c.applyFinishedTurn(ctx, run, turn, req.WorkerID, false)
				if applyErr != nil {
					return nil, applyErr
				}
				return result, ErrBudgetExhausted
			}
			reservedRun, _, err := c.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
				ExpectedRevision: run.Revision, Status: AgentRunStatusRunning,
				Summary: "Reserved capacity for a bounded agent turn", EventType: "budget.reserved",
				Actor: ActivityActor{Type: "worker", ID: req.WorkerID}, LeaseOwner: leaseOwner,
				BudgetReservation: &BudgetReservation{ID: turn.ID, Usage: reservationUsage, CreatedAt: c.activity.now()},
			})
			if err != nil {
				return nil, err
			}
			run = reservedRun
		}
	}
	outcome, runErr := runner.RunTurn(ctx, TurnExecutionContext{Run: cloneAgentRun(run), Turn: cloneAgentTurn(turn)})
	if errors.Is(runErr, ErrTurnHostUnavailable) {
		released, releaseErr := c.turns.ReleaseTurn(ctx, req.Scope, turn.ID, turn.Revision, req.WorkerID)
		if releaseErr != nil {
			return nil, releaseErr
		}
		retryAttempt := released.Revision / 2
		if retryAttempt < 1 {
			retryAttempt = 1
		}
		retryAt := c.activity.now().Add(hostedTurnRetryDelay(retryAttempt))
		requeued, event, transitionErr := c.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusSleeping, LeaseOwner: req.WorkerID,
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
	if runErr != nil {
		finish.Status = AgentTurnStatusFailed
		finish.Error = runErr.Error()
		finish.NextRunStatus = AgentRunStatusFailed
		finish.RunError = runErr.Error()
		finish.OutputSummary = "Bounded agent turn failed"
	} else if outcome == nil {
		executionErr = errors.New("turn runner returned no outcome")
		finish.Status = AgentTurnStatusFailed
		finish.Error = executionErr.Error()
		finish.NextRunStatus = AgentRunStatusFailed
		finish.RunError = finish.Error
		finish.OutputSummary = "Bounded agent turn failed"
	} else {
		if err := validateTurnOutcome(run.Status, outcome); err != nil {
			executionErr = err
			finish.Status = AgentTurnStatusFailed
			finish.Error = err.Error()
			finish.NextRunStatus = AgentRunStatusFailed
			finish.RunError = err.Error()
			finish.OutputSummary = "Bounded agent turn produced an invalid outcome"
		} else {
			finish.Status = AgentTurnStatusCompleted
			finish.Decisions = outcome.Decisions
			finish.RequestedActions = outcome.ProposedActions
			finish.OutputSummary = outcome.OutputSummary
			finish.Usage = outcome.Usage
			finish.ContinuationCheckpoint = outcome.ContinuationCheckpoint
			finish.NextRunStatus = outcome.NextRunStatus
			finish.WakeCondition = outcome.WakeCondition
			finish.RunOutput = outcome.RunOutput
			finish.RunError = outcome.RunError
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
		if state == BudgetStateExhausted {
			finish.NextRunStatus = AgentRunStatusPaused
			finish.OutputSummary = "Run paused after reaching its autonomous budget"
		}
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
	var budgetDelta *BudgetUsage
	if run.Budget != nil {
		if _, reserved := run.BudgetReservations[turn.ID]; reserved {
			delta := budgetUsageForTurn(turn.Usage)
			budgetDelta = &delta
		}
	}
	updated, event, err := c.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: turn.NextRunStatus, Summary: summary,
		Actor: ActivityActor{Type: "worker", ID: workerID}, Checkpoint: turn.ContinuationCheckpoint,
		WakeCondition: turn.WakeCondition, Output: turn.RunOutput, Error: turn.RunError,
		TurnID: turn.ID, AppliedTurn: turn.Sequence, CausationID: turn.ID,
		LeaseOwner:                leaseOwner,
		BudgetUsageDelta:          budgetDelta,
		SettleBudgetReservationID: budgetReservationID(run, turn),
	})
	if err != nil {
		return nil, err
	}
	return &AdvanceAgentRunResult{Run: updated, Turn: turn, Event: event, Reconciled: reconciled}, nil
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
	return nil
}
