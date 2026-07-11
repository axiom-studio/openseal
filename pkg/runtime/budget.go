package runtime

import (
	"errors"
	"fmt"
	"time"
)

var ErrBudgetExhausted = errors.New("run budget is exhausted")

type BudgetState string

const (
	BudgetStateActive    BudgetState = "active"
	BudgetStateWarning   BudgetState = "warning"
	BudgetStateExhausted BudgetState = "exhausted"
)

// BudgetPolicy is a portable, provider-neutral ceiling for autonomous work.
// Zero values are unlimited. Cost is represented in micros to keep accounting
// deterministic across stores and process restarts.
type BudgetPolicy struct {
	MaxTurns        int64 `json:"maxTurns,omitempty"`
	MaxInputTokens  int64 `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	MaxTotalTokens  int64 `json:"maxTotalTokens,omitempty"`
	MaxCostMicros   int64 `json:"maxCostMicros,omitempty"`
	MaxDurationMS   int64 `json:"maxDurationMs,omitempty"`
	MaxActions      int64 `json:"maxActions,omitempty"`
	WarningPermille int64 `json:"warningPermille,omitempty"`
}

func (p BudgetPolicy) Validate() error {
	if p.MaxTurns < 0 || p.MaxInputTokens < 0 || p.MaxOutputTokens < 0 || p.MaxTotalTokens < 0 ||
		p.MaxCostMicros < 0 || p.MaxDurationMS < 0 || p.MaxActions < 0 {
		return errors.New("budget limits cannot be negative")
	}
	if p.WarningPermille < 0 || p.WarningPermille > 1000 {
		return errors.New("budget warning threshold must be between 0 and 1000 permille")
	}
	return nil
}

type BudgetUsage struct {
	Turns        int64 `json:"turns,omitempty"`
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
	CostMicros   int64 `json:"costMicros,omitempty"`
	DurationMS   int64 `json:"durationMs,omitempty"`
	Actions      int64 `json:"actions,omitempty"`
}

type BudgetReservation struct {
	ID        string      `json:"id"`
	Usage     BudgetUsage `json:"usage"`
	CreatedAt time.Time   `json:"createdAt"`
}

func (r BudgetReservation) Validate() error {
	if r.ID == "" || r.CreatedAt.IsZero() {
		return errors.New("budget reservation id and creation time are required")
	}
	return r.Usage.Validate()
}

func (u BudgetUsage) Validate() error {
	if u.Turns < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.CostMicros < 0 || u.DurationMS < 0 || u.Actions < 0 {
		return errors.New("budget usage cannot be negative")
	}
	return nil
}

func (u BudgetUsage) Add(delta BudgetUsage) (BudgetUsage, error) {
	if err := u.Validate(); err != nil {
		return BudgetUsage{}, err
	}
	if err := delta.Validate(); err != nil {
		return BudgetUsage{}, err
	}
	return BudgetUsage{
		Turns: u.Turns + delta.Turns, InputTokens: u.InputTokens + delta.InputTokens,
		OutputTokens: u.OutputTokens + delta.OutputTokens, CostMicros: u.CostMicros + delta.CostMicros,
		DurationMS: u.DurationMS + delta.DurationMS, Actions: u.Actions + delta.Actions,
	}, nil
}

func EvaluateBudget(policy BudgetPolicy, usage BudgetUsage) (BudgetState, []string, error) {
	if err := policy.Validate(); err != nil {
		return "", nil, err
	}
	if err := usage.Validate(); err != nil {
		return "", nil, err
	}
	limits := []struct {
		name  string
		used  int64
		limit int64
	}{
		{"turns", usage.Turns, policy.MaxTurns},
		{"input_tokens", usage.InputTokens, policy.MaxInputTokens},
		{"output_tokens", usage.OutputTokens, policy.MaxOutputTokens},
		{"total_tokens", usage.InputTokens + usage.OutputTokens, policy.MaxTotalTokens},
		{"cost_micros", usage.CostMicros, policy.MaxCostMicros},
		{"duration_ms", usage.DurationMS, policy.MaxDurationMS},
		{"actions", usage.Actions, policy.MaxActions},
	}
	warningThreshold := policy.WarningPermille
	if warningThreshold == 0 {
		warningThreshold = 800
	}
	state := BudgetStateActive
	reasons := make([]string, 0)
	for _, item := range limits {
		if item.limit == 0 {
			continue
		}
		if item.used >= item.limit {
			state = BudgetStateExhausted
			reasons = append(reasons, fmt.Sprintf("%s reached %d of %d", item.name, item.used, item.limit))
			continue
		}
		if state != BudgetStateExhausted && item.used*1000 >= item.limit*warningThreshold {
			state = BudgetStateWarning
			reasons = append(reasons, fmt.Sprintf("%s reached %d of %d", item.name, item.used, item.limit))
		}
	}
	return state, reasons, nil
}

func EffectiveBudgetUsage(committed BudgetUsage, reservations map[string]BudgetReservation) (BudgetUsage, error) {
	effective := committed
	for id, reservation := range reservations {
		if reservation.ID != id {
			return BudgetUsage{}, errors.New("budget reservation key does not match its id")
		}
		var err error
		effective, err = effective.Add(reservation.Usage)
		if err != nil {
			return BudgetUsage{}, err
		}
	}
	return effective, nil
}

func BudgetWouldExceed(policy BudgetPolicy, usage BudgetUsage) (bool, []string, error) {
	if err := policy.Validate(); err != nil {
		return false, nil, err
	}
	if err := usage.Validate(); err != nil {
		return false, nil, err
	}
	limits := []struct {
		name  string
		used  int64
		limit int64
	}{
		{"turns", usage.Turns, policy.MaxTurns},
		{"input_tokens", usage.InputTokens, policy.MaxInputTokens},
		{"output_tokens", usage.OutputTokens, policy.MaxOutputTokens},
		{"total_tokens", usage.InputTokens + usage.OutputTokens, policy.MaxTotalTokens},
		{"cost_micros", usage.CostMicros, policy.MaxCostMicros},
		{"duration_ms", usage.DurationMS, policy.MaxDurationMS},
		{"actions", usage.Actions, policy.MaxActions},
	}
	reasons := make([]string, 0)
	for _, item := range limits {
		if item.limit > 0 && item.used > item.limit {
			reasons = append(reasons, fmt.Sprintf("%s would reach %d beyond %d", item.name, item.used, item.limit))
		}
	}
	return len(reasons) > 0, reasons, nil
}

func validateChildBudgetAllocation(parent *AgentRun, allocation *BudgetPolicy) error {
	if parent == nil {
		return ErrRunNotFound
	}
	if allocation != nil {
		if err := allocation.Validate(); err != nil {
			return fmt.Errorf("invalid child budget allocation: %w", err)
		}
	}
	if parent.BudgetPolicy == nil {
		return nil
	}
	if allocation == nil {
		return errors.New("a budgeted source run requires an explicit child budget allocation")
	}
	effective, err := EffectiveBudgetUsage(parent.BudgetUsage, parent.BudgetReservations)
	if err != nil {
		return err
	}
	existing := sumBudgetPolicies(parent.BudgetAllocations)
	for _, dimension := range budgetDimensions(*parent.BudgetPolicy, effective, addBudgetPolicies(existing, *allocation)) {
		if dimension.parentLimit == 0 {
			continue
		}
		remaining := dimension.parentLimit - dimension.used
		if remaining < 0 {
			remaining = 0
		}
		if dimension.allocation == 0 || dimension.allocation > remaining {
			return fmt.Errorf("child %s budget %d exceeds parent remaining capacity %d", dimension.name, dimension.allocation, remaining)
		}
	}
	return nil
}

func addRunBudgetAllocation(run *AgentRun, allocationID string, allocation *BudgetPolicy) error {
	if run == nil || run.BudgetPolicy == nil {
		return nil
	}
	if allocation == nil {
		return errors.New("a budgeted source run requires an explicit child budget allocation")
	}
	if _, exists := run.BudgetAllocations[allocationID]; exists {
		return errors.New("child budget allocation already exists")
	}
	if err := validateChildBudgetAllocation(run, allocation); err != nil {
		return err
	}
	if run.BudgetAllocations == nil {
		run.BudgetAllocations = make(map[string]BudgetPolicy)
	}
	run.BudgetAllocations[allocationID] = *cloneBudgetPolicy(allocation)
	return nil
}

func validateGroupedBudgetAllocations(parent *AgentRun, allocations []*BudgetPolicy) error {
	if parent == nil || parent.BudgetPolicy == nil {
		return nil
	}
	total := BudgetPolicy{}
	for _, allocation := range allocations {
		if allocation == nil {
			return errors.New("a budgeted source run requires explicit allocations for every child")
		}
		total.MaxTurns += allocation.MaxTurns
		total.MaxInputTokens += allocation.MaxInputTokens
		total.MaxOutputTokens += allocation.MaxOutputTokens
		total.MaxTotalTokens += allocation.MaxTotalTokens
		total.MaxCostMicros += allocation.MaxCostMicros
		total.MaxDurationMS += allocation.MaxDurationMS
		total.MaxActions += allocation.MaxActions
	}
	return validateChildBudgetAllocation(parent, &total)
}

type budgetDimension struct {
	name        string
	parentLimit int64
	used        int64
	allocation  int64
}

func budgetDimensions(parent BudgetPolicy, usage BudgetUsage, allocation BudgetPolicy) []budgetDimension {
	return []budgetDimension{
		{"turns", parent.MaxTurns, usage.Turns, allocation.MaxTurns},
		{"input_tokens", parent.MaxInputTokens, usage.InputTokens, allocation.MaxInputTokens},
		{"output_tokens", parent.MaxOutputTokens, usage.OutputTokens, allocation.MaxOutputTokens},
		{"total_tokens", parent.MaxTotalTokens, usage.InputTokens + usage.OutputTokens, allocation.MaxTotalTokens},
		{"cost_micros", parent.MaxCostMicros, usage.CostMicros, allocation.MaxCostMicros},
		{"duration_ms", parent.MaxDurationMS, usage.DurationMS, allocation.MaxDurationMS},
		{"actions", parent.MaxActions, usage.Actions, allocation.MaxActions},
	}
}

func sumBudgetPolicies(policies map[string]BudgetPolicy) BudgetPolicy {
	total := BudgetPolicy{}
	for _, policy := range policies {
		total = addBudgetPolicies(total, policy)
	}
	return total
}

func addBudgetPolicies(left, right BudgetPolicy) BudgetPolicy {
	return BudgetPolicy{
		MaxTurns:        left.MaxTurns + right.MaxTurns,
		MaxInputTokens:  left.MaxInputTokens + right.MaxInputTokens,
		MaxOutputTokens: left.MaxOutputTokens + right.MaxOutputTokens,
		MaxTotalTokens:  left.MaxTotalTokens + right.MaxTotalTokens,
		MaxCostMicros:   left.MaxCostMicros + right.MaxCostMicros,
		MaxDurationMS:   left.MaxDurationMS + right.MaxDurationMS,
		MaxActions:      left.MaxActions + right.MaxActions,
	}
}
