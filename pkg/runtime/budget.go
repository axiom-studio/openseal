package runtime

import (
	"encoding/json"
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
// Omitted JSON dimensions are unbounded; supplied limits must be positive.
// Programmatic zero values retain the same unbounded meaning. Cost is
// represented in micros to keep accounting deterministic across stores and
// process restarts.
type BudgetPolicy struct {
	MaxAttempts     int64 `json:"maxAttempts,omitempty"`
	MaxTurns        int64 `json:"maxTurns,omitempty"`
	MaxInputTokens  int64 `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	MaxTotalTokens  int64 `json:"maxTotalTokens,omitempty"`
	MaxCostMicros   int64 `json:"maxCostMicros,omitempty"`
	MaxDurationMS   int64 `json:"maxDurationMs,omitempty"`
	MaxActions      int64 `json:"maxActions,omitempty"`
	WarningPermille int64 `json:"warningPermille,omitempty"`
}

// UnmarshalJSON preserves the only portable interpretation of an omitted
// ceiling: that dimension is unbounded. An explicitly supplied zero is
// rejected because it is otherwise indistinguishable from omission after Go
// decoding and can be misread by an execution host as either "unbounded" or
// "exhausted". Callers that do not want to bound a dimension must omit it.
func (p *BudgetPolicy) UnmarshalJSON(data []byte) error {
	if p == nil {
		return errors.New("budget policy is required")
	}
	type wireBudgetPolicy BudgetPolicy
	var decoded wireBudgetPolicy
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	allowed := map[string]struct{}{"warningPermille": {}}
	for _, dimension := range budgetLimitFields(BudgetPolicy(decoded)) {
		allowed[dimension.jsonName] = struct{}{}
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("json: unknown field %q", name)
		}
	}
	for _, dimension := range budgetLimitFields(BudgetPolicy(decoded)) {
		if _, specified := fields[dimension.jsonName]; specified && dimension.value == 0 {
			return fmt.Errorf("budget %s must be positive when specified; omit it for an unbounded dimension", dimension.jsonName)
		}
	}
	*p = BudgetPolicy(decoded)
	return p.Validate()
}

func (p BudgetPolicy) Validate() error {
	for _, dimension := range budgetLimitFields(p) {
		if dimension.value < 0 {
			return fmt.Errorf("budget %s cannot be negative", dimension.jsonName)
		}
	}
	if p.WarningPermille < 0 || p.WarningPermille > 1000 {
		return errors.New("budget warning threshold must be between 0 and 1000 permille")
	}
	return nil
}

type budgetLimitField struct {
	jsonName string
	value    int64
}

func budgetLimitFields(policy BudgetPolicy) []budgetLimitField {
	return []budgetLimitField{
		{"maxAttempts", policy.MaxAttempts},
		{"maxTurns", policy.MaxTurns},
		{"maxInputTokens", policy.MaxInputTokens},
		{"maxOutputTokens", policy.MaxOutputTokens},
		{"maxTotalTokens", policy.MaxTotalTokens},
		{"maxCostMicros", policy.MaxCostMicros},
		{"maxDurationMs", policy.MaxDurationMS},
		{"maxActions", policy.MaxActions},
	}
}

type BudgetUsage struct {
	Attempts     int64 `json:"attempts,omitempty"`
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
	if u.Attempts < 0 || u.Turns < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.CostMicros < 0 || u.DurationMS < 0 || u.Actions < 0 {
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
		Attempts: u.Attempts + delta.Attempts, Turns: u.Turns + delta.Turns, InputTokens: u.InputTokens + delta.InputTokens,
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
		{"attempts", usage.Attempts, policy.MaxAttempts},
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
		{"attempts", usage.Attempts, policy.MaxAttempts},
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
	if parent.Budget == nil {
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
	for _, dimension := range budgetDimensions(*parent.Budget, effective, addBudgetPolicies(existing, *allocation)) {
		if dimension.parentLimit == 0 {
			continue
		}
		remaining := dimension.parentLimit - dimension.used
		if remaining < 0 {
			remaining = 0
		}
		if dimension.allocation == 0 || dimension.allocation > remaining {
			return fmt.Errorf("%w: child %s budget %d exceeds parent remaining capacity %d", ErrBudgetExhausted, dimension.name, dimension.allocation, remaining)
		}
	}
	return nil
}

func addRunBudgetAllocation(run *AgentRun, allocationID string, allocation *BudgetPolicy) error {
	if run == nil || run.Budget == nil {
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

func reserveRunBudget(run *AgentRun, reservation BudgetReservation) error {
	if run == nil || run.Budget == nil {
		return nil
	}
	if err := reservation.Validate(); err != nil {
		return err
	}
	if _, exists := run.BudgetReservations[reservation.ID]; exists {
		return errors.New("budget reservation already exists")
	}
	if run.BudgetReservations == nil {
		run.BudgetReservations = make(map[string]BudgetReservation)
	}
	run.BudgetReservations[reservation.ID] = reservation
	effective, err := EffectiveBudgetUsage(run.BudgetUsage, run.BudgetReservations)
	if err != nil {
		delete(run.BudgetReservations, reservation.ID)
		return err
	}
	exceeded, _, err := BudgetWouldExceed(*run.Budget, effective)
	if err != nil || exceeded {
		delete(run.BudgetReservations, reservation.ID)
		if err != nil {
			return err
		}
		return ErrBudgetExhausted
	}
	state, _, err := EvaluateBudget(*run.Budget, effective)
	if err != nil {
		delete(run.BudgetReservations, reservation.ID)
		return err
	}
	run.BudgetState = state
	return nil
}

func settleRunBudgetReservation(run *AgentRun, reservationID string, usage BudgetUsage) error {
	if run == nil || run.Budget == nil {
		return nil
	}
	if _, exists := run.BudgetReservations[reservationID]; !exists {
		return errors.New("budget reservation does not exist")
	}
	updated, err := run.BudgetUsage.Add(usage)
	if err != nil {
		return err
	}
	delete(run.BudgetReservations, reservationID)
	effective, err := EffectiveBudgetUsage(updated, run.BudgetReservations)
	if err != nil {
		return err
	}
	state, _, err := EvaluateBudget(*run.Budget, effective)
	if err != nil {
		return err
	}
	run.BudgetUsage = updated
	run.BudgetState = state
	return nil
}

func releaseRunBudgetReservation(run *AgentRun, reservationID string) error {
	if run == nil || run.Budget == nil {
		return nil
	}
	if _, exists := run.BudgetReservations[reservationID]; !exists {
		return errors.New("budget reservation does not exist")
	}
	delete(run.BudgetReservations, reservationID)
	effective, err := EffectiveBudgetUsage(run.BudgetUsage, run.BudgetReservations)
	if err != nil {
		return err
	}
	state, _, err := EvaluateBudget(*run.Budget, effective)
	if err != nil {
		return err
	}
	run.BudgetState = state
	return nil
}

func validateGroupedBudgetAllocations(parent *AgentRun, allocations []*BudgetPolicy) error {
	if parent == nil || parent.Budget == nil {
		return nil
	}
	total := BudgetPolicy{}
	for _, allocation := range allocations {
		if allocation == nil {
			return errors.New("a budgeted source run requires explicit allocations for every child")
		}
		total.MaxTurns += allocation.MaxTurns
		total.MaxAttempts += allocation.MaxAttempts
		total.MaxInputTokens += allocation.MaxInputTokens
		total.MaxOutputTokens += allocation.MaxOutputTokens
		total.MaxTotalTokens += allocation.MaxTotalTokens
		total.MaxCostMicros += allocation.MaxCostMicros
		total.MaxDurationMS += allocation.MaxDurationMS
		total.MaxActions += allocation.MaxActions
	}
	return validateChildBudgetAllocation(parent, &total)
}

func validateObjectiveRunBudget(objective *Objective, allocation *BudgetPolicy) error {
	if objective == nil || objective.Budget == nil {
		return nil
	}
	if allocation == nil {
		return errors.New("a budgeted objective requires an explicit run budget allocation")
	}
	if err := allocation.Validate(); err != nil {
		return err
	}
	combined := addBudgetPolicies(sumBudgetPolicies(objective.BudgetAllocations), *allocation)
	return validatePolicyWithinLimit(*objective.Budget, combined, "run", true)
}

func validatePolicyAllocations(limit BudgetPolicy, allocations map[string]BudgetPolicy) error {
	for id, allocation := range allocations {
		if id == "" {
			return errors.New("budget allocation id is required")
		}
		if err := allocation.Validate(); err != nil {
			return fmt.Errorf("budget allocation %s: %w", id, err)
		}
	}
	return validatePolicyWithinLimit(limit, sumBudgetPolicies(allocations), "aggregate", false)
}

func validatePolicyWithinLimit(limit, allocated BudgetPolicy, subject string, requireBounded bool) error {
	for _, dimension := range budgetDimensions(limit, BudgetUsage{}, allocated) {
		if dimension.parentLimit > 0 && (requireBounded && dimension.allocation == 0 || dimension.allocation > dimension.parentLimit) {
			return fmt.Errorf("%w: %s %s budget %d exceeds limit %d", ErrBudgetExhausted, subject, dimension.name, dimension.allocation, dimension.parentLimit)
		}
	}
	return nil
}

func allocateObjectiveRunBudget(objective *Objective, run *AgentRun, now time.Time) (*Objective, error) {
	if objective == nil || run == nil || run.ParentRunID != "" || objective.Budget == nil {
		return objective, nil
	}
	if err := validateObjectiveRunBudget(objective, run.Budget); err != nil {
		return nil, err
	}
	updated := cloneObjective(objective)
	if updated.BudgetAllocations == nil {
		updated.BudgetAllocations = make(map[string]BudgetPolicy)
	}
	if _, exists := updated.BudgetAllocations[run.ID]; exists {
		return nil, ErrRunIdempotency
	}
	updated.BudgetAllocations[run.ID] = *cloneBudgetPolicy(run.Budget)
	updated.Revision++
	updated.UpdatedAt = now
	if err := updated.Validate(); err != nil {
		return nil, err
	}
	return updated, nil
}

type budgetDimension struct {
	name        string
	parentLimit int64
	used        int64
	allocation  int64
}

func budgetDimensions(parent BudgetPolicy, usage BudgetUsage, allocation BudgetPolicy) []budgetDimension {
	return []budgetDimension{
		{"attempts", parent.MaxAttempts, usage.Attempts, allocation.MaxAttempts},
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
		MaxAttempts:     left.MaxAttempts + right.MaxAttempts,
		MaxTurns:        left.MaxTurns + right.MaxTurns,
		MaxInputTokens:  left.MaxInputTokens + right.MaxInputTokens,
		MaxOutputTokens: left.MaxOutputTokens + right.MaxOutputTokens,
		MaxTotalTokens:  left.MaxTotalTokens + right.MaxTotalTokens,
		MaxCostMicros:   left.MaxCostMicros + right.MaxCostMicros,
		MaxDurationMS:   left.MaxDurationMS + right.MaxDurationMS,
		MaxActions:      left.MaxActions + right.MaxActions,
	}
}
