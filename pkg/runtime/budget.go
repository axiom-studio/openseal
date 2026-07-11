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
