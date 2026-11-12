package modelstep

import (
	"fmt"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
)

// Kernel floors for one child Run that a bounded parent funds through a
// control tool (fork, delegation, task). The Turns dimension counts model
// steps (D12), so the child minimum is the old two-Turn floor ×4. Unbounded
// parent dimensions keep unbounded children.
const (
	controlMinimumChildAttempts     int64 = 3
	controlMinimumChildTurns        int64 = 8
	controlMinimumChildInputTokens  int64 = 16384
	controlMinimumChildOutputTokens int64 = capability.HostedMinimumChildOutputTokens
	controlMinimumChildTotalTokens  int64 = controlMinimumChildInputTokens + controlMinimumChildOutputTokens
	controlMinimumChildDurationMS   int64 = 180000
	controlMinimumChildActions      int64 = 1
)

// ControlBudgetErrorCode is the stable tool error code returned when a bounded
// Run cannot fund the child Runs a control tool asked for.
const ControlBudgetErrorCode = "budget_insufficient"

// ControlBudgetInsufficientError reports that a bounded Run's remaining budget
// cannot fund Children more child Runs. Control tools turn it into the tool
// error {"error": Code(), "message": Message()}.
type ControlBudgetInsufficientError struct {
	Children int
	Cause    error
}

func (e *ControlBudgetInsufficientError) Error() string {
	if e.Cause == nil {
		return e.Message()
	}
	return e.Message() + ": " + e.Cause.Error()
}

func (e *ControlBudgetInsufficientError) Unwrap() error { return e.Cause }

func (e *ControlBudgetInsufficientError) Code() string { return ControlBudgetErrorCode }

// Message is the one sentence shown to the model.
func (e *ControlBudgetInsufficientError) Message() string {
	return fmt.Sprintf("This Run's remaining budget cannot fund %d more child Runs.", e.Children)
}

// ToolError is the tool result payload for the model.
func (e *ControlBudgetInsufficientError) ToolError() map[string]interface{} {
	return map[string]interface{}{"error": e.Code(), "message": e.Message()}
}

// controlRunBudget projects the capacity visible to the step stepID of run.
// The step's own reservation is reported separately and is not subtracted
// from Remaining. An unbounded Run has no budget (nil, nil).
func ControlRunBudget(run *models.AgentRun, stepID string) (*HostedRunBudget, error) {
	if run == nil || run.Budget == nil {
		return nil, nil
	}
	otherReservations := make(map[string]models.BudgetReservation, len(run.BudgetReservations))
	currentReservation := models.BudgetUsage{}
	for id, reservation := range run.BudgetReservations {
		if id == stepID {
			currentReservation = reservation.Usage
			continue
		}
		otherReservations[id] = reservation
	}
	effective, err := models.EffectiveBudgetUsage(run.BudgetUsage, otherReservations)
	if err != nil {
		return nil, fmt.Errorf("project control Run budget: %w", err)
	}
	policy := *models.CloneBudgetPolicy(run.Budget)
	allocated := models.SumBudgetPolicies(run.BudgetAllocations)
	remaining := models.BudgetPolicy{
		MaxAttempts:     remainingBudgetDimension(policy.MaxAttempts, effective.Attempts, allocated.MaxAttempts),
		MaxTurns:        remainingBudgetDimension(policy.MaxTurns, effective.Turns, allocated.MaxTurns),
		MaxInputTokens:  remainingBudgetDimension(policy.MaxInputTokens, effective.InputTokens, allocated.MaxInputTokens),
		MaxOutputTokens: remainingBudgetDimension(policy.MaxOutputTokens, effective.OutputTokens, allocated.MaxOutputTokens),
		MaxTotalTokens:  remainingBudgetDimension(policy.MaxTotalTokens, effective.InputTokens+effective.OutputTokens, allocated.MaxTotalTokens),
		MaxCostMicros:   remainingBudgetDimension(policy.MaxCostMicros, effective.CostMicros, allocated.MaxCostMicros),
		MaxDurationMS:   remainingBudgetDimension(policy.MaxDurationMS, effective.DurationMS, allocated.MaxDurationMS),
		MaxActions:      remainingBudgetDimension(policy.MaxActions, effective.Actions, allocated.MaxActions),
	}
	return &HostedRunBudget{
		Policy: policy, CommittedUsage: run.BudgetUsage, EffectiveUsage: effective,
		Allocated: allocated, TurnReservation: currentReservation, Remaining: remaining,
		MinimumChild: controlMinimumChildBudget(policy),
	}, nil
}

// controlMinimumChildBudget is the floor for one child of a parent bounded by
// policy. A dimension the parent leaves unbounded stays unbounded (zero).
func controlMinimumChildBudget(policy models.BudgetPolicy) models.BudgetPolicy {
	floor := func(limit, minimum int64) int64 {
		if limit == 0 {
			return 0
		}
		return minimum
	}
	return models.BudgetPolicy{
		MaxAttempts:     floor(policy.MaxAttempts, controlMinimumChildAttempts),
		MaxTurns:        floor(policy.MaxTurns, controlMinimumChildTurns),
		MaxInputTokens:  floor(policy.MaxInputTokens, controlMinimumChildInputTokens),
		MaxOutputTokens: floor(policy.MaxOutputTokens, controlMinimumChildOutputTokens),
		MaxTotalTokens:  floor(policy.MaxTotalTokens, controlMinimumChildTotalTokens),
		MaxCostMicros:   floor(policy.MaxCostMicros, 1),
		MaxDurationMS:   floor(policy.MaxDurationMS, controlMinimumChildDurationMS),
		MaxActions:      floor(policy.MaxActions, controlMinimumChildActions),
	}
}

// controlChildBudgetFloor rejects an allocation below budget.MinimumChild in
// any bounded dimension.
func ControlChildBudgetFloor(allocation *models.BudgetPolicy, budget *HostedRunBudget) error {
	if budget == nil {
		return nil
	}
	actual := models.BudgetPolicy{}
	if allocation != nil {
		actual = *allocation
	}
	for _, dimension := range []struct {
		name          string
		actual, floor int64
	}{
		{"maxAttempts", actual.MaxAttempts, budget.MinimumChild.MaxAttempts},
		{"maxTurns", actual.MaxTurns, budget.MinimumChild.MaxTurns},
		{"maxInputTokens", actual.MaxInputTokens, budget.MinimumChild.MaxInputTokens},
		{"maxOutputTokens", actual.MaxOutputTokens, budget.MinimumChild.MaxOutputTokens},
		{"maxTotalTokens", actual.MaxTotalTokens, budget.MinimumChild.MaxTotalTokens},
		{"maxCostMicros", actual.MaxCostMicros, budget.MinimumChild.MaxCostMicros},
		{"maxDurationMs", actual.MaxDurationMS, budget.MinimumChild.MaxDurationMS},
		{"maxActions", actual.MaxActions, budget.MinimumChild.MaxActions},
	} {
		if dimension.floor > 0 && dimension.actual < dimension.floor {
			return fmt.Errorf("%s %d is below the child minimum %d", dimension.name, dimension.actual, dimension.floor)
		}
	}
	return nil
}

// controlChildBudgetCapacity rejects allocations whose sum exceeds the
// remaining capacity of any bounded dimension.
func controlChildBudgetCapacity(allocations []*models.BudgetPolicy, budget *HostedRunBudget) error {
	if budget == nil {
		return nil
	}
	for _, dimension := range []struct {
		name      string
		bounded   bool
		remaining int64
		value     func(models.BudgetPolicy) int64
	}{
		{"maxAttempts", budget.Policy.MaxAttempts > 0, budget.Remaining.MaxAttempts, func(policy models.BudgetPolicy) int64 { return policy.MaxAttempts }},
		{"maxTurns", budget.Policy.MaxTurns > 0, budget.Remaining.MaxTurns, func(policy models.BudgetPolicy) int64 { return policy.MaxTurns }},
		{"maxInputTokens", budget.Policy.MaxInputTokens > 0, budget.Remaining.MaxInputTokens, func(policy models.BudgetPolicy) int64 { return policy.MaxInputTokens }},
		{"maxOutputTokens", budget.Policy.MaxOutputTokens > 0, budget.Remaining.MaxOutputTokens, func(policy models.BudgetPolicy) int64 { return policy.MaxOutputTokens }},
		{"maxTotalTokens", budget.Policy.MaxTotalTokens > 0, budget.Remaining.MaxTotalTokens, func(policy models.BudgetPolicy) int64 { return policy.MaxTotalTokens }},
		{"maxCostMicros", budget.Policy.MaxCostMicros > 0, budget.Remaining.MaxCostMicros, func(policy models.BudgetPolicy) int64 { return policy.MaxCostMicros }},
		{"maxDurationMs", budget.Policy.MaxDurationMS > 0, budget.Remaining.MaxDurationMS, func(policy models.BudgetPolicy) int64 { return policy.MaxDurationMS }},
		{"maxActions", budget.Policy.MaxActions > 0, budget.Remaining.MaxActions, func(policy models.BudgetPolicy) int64 { return policy.MaxActions }},
	} {
		if !dimension.bounded {
			continue
		}
		total := int64(0)
		for _, allocation := range allocations {
			actual := int64(0)
			if allocation != nil {
				actual = dimension.value(*allocation)
			}
			if actual > dimension.remaining-total {
				return fmt.Errorf("%s allocations exceed remaining capacity %d", dimension.name, dimension.remaining)
			}
			total += actual
		}
	}
	return nil
}

// controlTaskBudget is the budget of a conversation task started by a bounded
// Run: every bounded dimension inherits the remaining capacity. An unbounded
// Run (nil budget) starts an unbounded task (empty policy).
func ControlTaskBudget(budget *HostedRunBudget) *models.BudgetPolicy {
	completed := &models.BudgetPolicy{}
	if budget == nil {
		return completed
	}
	for _, dimension := range []struct {
		limit, remaining int64
		value            *int64
	}{
		{budget.Policy.MaxAttempts, budget.Remaining.MaxAttempts, &completed.MaxAttempts},
		{budget.Policy.MaxTurns, budget.Remaining.MaxTurns, &completed.MaxTurns},
		{budget.Policy.MaxInputTokens, budget.Remaining.MaxInputTokens, &completed.MaxInputTokens},
		{budget.Policy.MaxOutputTokens, budget.Remaining.MaxOutputTokens, &completed.MaxOutputTokens},
		{budget.Policy.MaxTotalTokens, budget.Remaining.MaxTotalTokens, &completed.MaxTotalTokens},
		{budget.Policy.MaxCostMicros, budget.Remaining.MaxCostMicros, &completed.MaxCostMicros},
		{budget.Policy.MaxDurationMS, budget.Remaining.MaxDurationMS, &completed.MaxDurationMS},
		{budget.Policy.MaxActions, budget.Remaining.MaxActions, &completed.MaxActions},
	} {
		if dimension.limit > 0 {
			*dimension.value = dimension.remaining
		}
	}
	return completed
}

// controlBranchBudgets allocates the budgets of n child Runs that the step
// stepID of run starts together. Unbounded Runs (the prod default) give every
// child a nil, unbounded budget. A bounded Run gives each child the minimum
// child budget and fails with *ControlBudgetInsufficientError when its
// remaining capacity cannot fund all n.
func ControlBranchBudgets(run *models.AgentRun, stepID string, n int) ([]*models.BudgetPolicy, error) {
	if n < 0 {
		return nil, fmt.Errorf("child Run count %d is negative", n)
	}
	allocations := make([]*models.BudgetPolicy, n)
	if run == nil || run.Budget == nil {
		return allocations, nil
	}
	budget, err := ControlRunBudget(run, stepID)
	if err != nil {
		return nil, err
	}
	for index := range allocations {
		minimum := budget.MinimumChild
		allocations[index] = &minimum
	}
	if err := controlChildBudgetCapacity(allocations, budget); err != nil {
		return nil, &ControlBudgetInsufficientError{Children: n, Cause: err}
	}
	return allocations, nil
}
