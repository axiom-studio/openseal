package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Per-step budget (D12). A bounded Run reserves capacity for every step
// before its producer runs, keyed by the step id, and settles that
// reservation when the step is applied. Unbounded Runs (run.Budget == nil,
// every prod chat) reserve nothing.

// Step failure codes written by the budget phase.
const (
	StepFailureBudgetExhausted = "budget_exhausted"
	StepFailureDurationBudget  = "duration_budget"
)

// Model step input estimation. The constants keep the values of the old
// hosted-turn protocol: a fixed reserve for host instructions, provider tool
// framing and tokenizer variance, two UTF-8 bytes per token for the request
// JSON, and a fixed reserve per image.
const (
	stepBudgetReservedSummary           = "Reserved capacity for a bounded agent step"
	stepBudgetReconciledSummary         = "Reconciled capacity for a retrying bounded agent step"
	stepBudgetAdmissionSummary          = "Run paused before exceeding its autonomous budget"
	stepDurationExpiredSummary          = "Run paused before exceeding its autonomous duration budget"
	stepDurationExceededSummary         = "Run paused after reaching its autonomous duration budget"
	stepBudgetExhaustedAfterStepSummary = "Run paused after reaching its autonomous budget"
)

// runTransitioner is the Run write the budget phase needs
// (RunActivityService.TransitionRun).
type runTransitioner interface {
	TransitionRun(ctx context.Context, scope Scope, runID string, req RunTransitionRequest) (*AgentRun, *ActivityEvent, error)
}

// reserveStepBudget reserves the step's capacity on a bounded Run:
// binding.BudgetReservation plus the producer's plan (StepBudgetPlanner),
// with Turns forced to 1. It returns the updated Run on success. When the
// reservation does not fit, it returns the unchanged Run and a complete
// admission (TurnID = step.ID, EvaluatedAt = now); the caller then cancels the
// step (stepBudgetAdmissionFinish) and pauses the Run (stepBudgetPause). An
// unbounded Run, or a reservation equal to the one already held for the step,
// is returned unchanged with no write.
func reserveStepBudget(ctx context.Context, runs runTransitioner, now time.Time, run *AgentRun, step *AgentTurn, binding *TurnRunnerBinding, in StepInput, workerID string) (*AgentRun, *BudgetAdmission, error) {
	if run == nil || step == nil {
		return nil, nil, errors.New("step budget reservation requires a Run and a step")
	}
	if run.Budget == nil {
		return run, nil, nil
	}
	refuse := func(admission BudgetAdmission) (*AgentRun, *BudgetAdmission, error) {
		admission.TurnID = step.ID
		admission.EvaluatedAt = now
		return run, &admission, nil
	}
	usage := BudgetUsage{}
	if binding != nil {
		usage = binding.BudgetReservation
	}
	var producer StepProducer
	if binding != nil {
		producer = binding.Producer
	}
	if planner, ok := producer.(StepBudgetPlanner); ok {
		planIn := in
		planIn.Run = cloneAgentRun(run)
		planIn.Step = cloneAgentTurn(step)
		planned, err := planner.PlanStepBudget(ctx, planIn)
		if err != nil {
			if !errors.Is(err, ErrBudgetExhausted) {
				return nil, nil, fmt.Errorf("plan step budget: %w", err)
			}
			var admissionErr *BudgetAdmissionError
			if errors.As(err, &admissionErr) {
				return refuse(admissionErr.Admission)
			}
			return refuse(BudgetAdmission{Reason: BudgetAdmissionHostedInput, Dimension: "unknown"})
		}
		if usage, err = usage.Add(planned); err != nil {
			return nil, nil, err
		}
	}
	usage.Turns = 1
	if err := usage.Validate(); err != nil {
		return nil, nil, err
	}
	existing, reserved := run.BudgetReservations[step.ID]
	if reserved && existing.Usage == usage {
		return run, nil, nil
	}
	effective, err := EffectiveBudgetUsage(run.BudgetUsage, otherStepReservations(run, step.ID))
	if err != nil {
		return nil, nil, err
	}
	projected, err := effective.Add(usage)
	if err != nil {
		return nil, nil, err
	}
	exceeded, _, err := BudgetWouldExceed(*run.Budget, projected)
	if err != nil {
		return nil, nil, err
	}
	if exceeded {
		return refuse(budgetReservationAdmission(*run.Budget, effective, usage))
	}
	if runs == nil {
		return nil, nil, errors.New("step budget reservation requires a Run transitioner")
	}
	reservation := &BudgetReservation{ID: step.ID, Usage: usage, CreatedAt: now}
	transition := RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning,
		Summary: stepBudgetReservedSummary, EventType: "budget.reserved",
		Actor: ActivityActor{Type: "worker", ID: workerID}, LeaseOwner: stepLeaseOwner(run, workerID),
		BudgetReservation: reservation,
	}
	if reserved {
		reservation.CreatedAt = existing.CreatedAt
		transition.Summary = stepBudgetReconciledSummary
		transition.EventType = "budget.reservation_reconciled"
		transition.BudgetReservation = nil
		transition.ReplaceBudgetReservation = reservation
	}
	updated, _, err := runs.TransitionRun(ctx, run.Scope, run.ID, transition)
	if err != nil {
		return nil, nil, err
	}
	return updated, nil, nil
}

// stepLeaseOwner fences a Run write on the worker's lease when the Run is
// leased.
func stepLeaseOwner(run *AgentRun, workerID string) string {
	if run == nil || run.LeaseOwner == "" {
		return ""
	}
	return workerID
}

func otherStepReservations(run *AgentRun, stepID string) map[string]BudgetReservation {
	others := make(map[string]BudgetReservation, len(run.BudgetReservations))
	for id, reservation := range run.BudgetReservations {
		if id != stepID {
			others[id] = reservation
		}
	}
	return others
}

// stepBudgetAdmissionFinish cancels a step whose reservation was refused.
func stepBudgetAdmissionFinish(step *AgentTurn, workerID string, admission BudgetAdmission) FinishAgentTurnRequest {
	return FinishAgentTurnRequest{
		ExpectedRevision: step.Revision, Status: AgentTurnStatusCanceled, WorkerID: workerID,
		OutputSummary: stepBudgetAdmissionSummary, BudgetAdmission: &admission,
		FailureCode: StepFailureBudgetExhausted,
	}
}

// stepBudgetPause is the budget exit transition (Run paused,
// budget.exhausted). The payload is {admission} for a refused reservation,
// otherwise {dimension, used, limit} for each exhausted dimension's first
// entry. The caller adds the step settlement (applyStep).
func stepBudgetPause(run *AgentRun, workerID, summary string, admission *BudgetAdmission, dimension string) RunTransitionRequest {
	if summary == "" {
		summary = stepBudgetAdmissionSummary
	}
	payload := map[string]interface{}{"budgetState": BudgetStateExhausted}
	if admission != nil {
		payload["admission"] = *admission
	} else if dimension != "" {
		payload["dimension"] = dimension
		if used, limit, ok := budgetDimensionUsage(run, dimension); ok {
			payload["used"] = used
			payload["limit"] = limit
		}
	}
	return RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusPaused, Summary: summary,
		EventType: "budget.exhausted", Actor: ActivityActor{Type: "worker", ID: workerID},
		LeaseOwner: stepLeaseOwner(run, workerID), BudgetAdmission: admission, Payload: payload,
	}
}

// budgetDimensionUsage reports committed usage and the limit of one named
// budget dimension (the names of budgetReservationAdmission).
func budgetDimensionUsage(run *AgentRun, dimension string) (used, limit int64, ok bool) {
	if run == nil || run.Budget == nil {
		return 0, 0, false
	}
	u, p := run.BudgetUsage, *run.Budget
	switch dimension {
	case "attempts":
		return u.Attempts, p.MaxAttempts, true
	case "turns":
		return u.Turns, p.MaxTurns, true
	case "input_tokens":
		return u.InputTokens, p.MaxInputTokens, true
	case "output_tokens":
		return u.OutputTokens, p.MaxOutputTokens, true
	case "total_tokens":
		return u.InputTokens + u.OutputTokens, p.MaxTotalTokens, true
	case "cost_micros":
		return u.CostMicros, p.MaxCostMicros, true
	case "duration_ms":
		return u.DurationMS, p.MaxDurationMS, true
	case "actions":
		return u.Actions, p.MaxActions, true
	}
	return 0, 0, false
}

// stepDurationDeadline bounds the step by the Run's remaining duration
// budget (committed duration plus other steps' reserved duration). ok is
// false when nothing is left: the caller cancels the step and pauses with
// {dimension: "duration_ms"} without starting the producer. chargeMS is the
// remaining duration the deadline was set from (0 when unbounded); a step
// cut by the deadline is charged at least that much.
func stepDurationDeadline(ctx context.Context, run *AgentRun, stepID string) (context.Context, context.CancelFunc, int64, bool) {
	if run == nil || run.Budget == nil || run.Budget.MaxDurationMS <= 0 {
		stepCtx, cancel := context.WithCancel(ctx)
		return stepCtx, cancel, 0, true
	}
	remainingMS := run.Budget.MaxDurationMS - run.BudgetUsage.DurationMS
	for id, reservation := range run.BudgetReservations {
		if id != stepID {
			remainingMS -= reservation.Usage.DurationMS
		}
	}
	if remainingMS <= 0 {
		return ctx, func() {}, 0, false
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(remainingMS)*time.Millisecond)
	return stepCtx, cancel, remainingMS, true
}

// stepDurationExpired reports whether stepCtx ended because of the duration
// deadline itself, not because the parent (lease, shutdown) ended.
func stepDurationExpired(parent, stepCtx context.Context) bool {
	return parent.Err() == nil && errors.Is(stepCtx.Err(), context.DeadlineExceeded)
}

// stepDurationExpiredFinish cancels a step that the duration deadline cut
// before it started (elapsedMS 0) or during the producer. Nothing the
// producer returned is appended, but its valid usage stays chargeable and the
// duration is at least the deadline's chargeMS.
func stepDurationExpiredFinish(step *AgentTurn, workerID string, usage TurnUsage, elapsedMS, chargeMS int64) FinishAgentTurnRequest {
	if usage.Validate() != nil {
		usage = TurnUsage{}
	}
	if elapsedMS > 0 || chargeMS > 0 {
		usage.DurationMS = max(usage.DurationMS, elapsedMS, chargeMS)
	}
	summary := stepDurationExceededSummary
	if elapsedMS == 0 && chargeMS == 0 {
		summary = stepDurationExpiredSummary
	}
	return FinishAgentTurnRequest{
		ExpectedRevision: step.Revision, Status: AgentTurnStatusCanceled, WorkerID: workerID,
		OutputSummary: summary, Usage: usage, FailureCode: StepFailureDurationBudget,
	}
}

// budgetUsageForStep converts a step's usage into Run budget usage. Turns
// counts steps that ran a producer (D12): a step canceled before its
// producer ran charges no Turn.
func budgetUsageForStep(usage TurnUsage, produced bool) BudgetUsage {
	costMicros := int64(0)
	if usage.Cost > 0 {
		costMicros = int64(math.Round(usage.Cost * 1_000_000))
	}
	turns := int64(0)
	if produced {
		turns = 1
	}
	return BudgetUsage{
		Turns: turns, InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens),
		CostMicros: costMicros, DurationMS: usage.DurationMS, Actions: int64(usage.NativeOperations),
	}
}

// stepExhaustsBudget reports whether settling step leaves the bounded Run
// exhausted (other steps' reservations still count). The caller pauses a Run
// that would otherwise continue.
func stepExhaustsBudget(run *AgentRun, step *AgentTurn, produced bool) bool {
	if run == nil || step == nil || run.Budget == nil {
		return false
	}
	usage, err := run.BudgetUsage.Add(budgetUsageForStep(step.Usage, produced))
	if err != nil {
		return false
	}
	effective, err := EffectiveBudgetUsage(usage, otherStepReservations(run, step.ID))
	if err != nil {
		return false
	}
	state, _, err := EvaluateBudget(*run.Budget, effective)
	return err == nil && state == BudgetStateExhausted
}

// stepReservationID is the reservation to settle when step is applied: the
// step id when a bounded Run holds a reservation for it, else "".
func stepReservationID(run *AgentRun, step *AgentTurn) string {
	if run == nil || step == nil || run.Budget == nil {
		return ""
	}
	if _, reserved := run.BudgetReservations[step.ID]; !reserved {
		return ""
	}
	return step.ID
}

// fillStepBudgetSettlement sets the budget fields of applyStep's transition:
// ActivityUsageDelta (always, when non-zero), BudgetUsageDelta and
// SettleBudgetReservationID (bounded and reserved only) and the step's
// BudgetAdmission. A pause on an exhausted bounded budget is marked
// budget.exhausted.
func fillStepBudgetSettlement(req *RunTransitionRequest, run *AgentRun, step *AgentTurn, produced bool) {
	if req == nil || run == nil || step == nil {
		return
	}
	delta := budgetUsageForStep(step.Usage, produced)
	if delta != (BudgetUsage{}) {
		activity := delta
		req.ActivityUsageDelta = &activity
	}
	if id := stepReservationID(run, step); id != "" {
		budget := delta
		req.BudgetUsageDelta = &budget
		req.SettleBudgetReservationID = id
	}
	if step.BudgetAdmission != nil {
		admission := *step.BudgetAdmission
		req.BudgetAdmission = &admission
		req.EventType = "budget.exhausted"
		setStepBudgetPayload(req, "admission", admission)
	} else if req.Status == AgentRunStatusPaused && stepExhaustsBudget(run, step, produced) {
		req.EventType = "budget.exhausted"
		setStepBudgetPayload(req, "", nil)
	}
}

func setStepBudgetPayload(req *RunTransitionRequest, key string, value interface{}) {
	if req.Payload == nil {
		req.Payload = map[string]interface{}{}
	}
	req.Payload["budgetState"] = BudgetStateExhausted
	if key != "" {
		req.Payload[key] = value
	}
}
