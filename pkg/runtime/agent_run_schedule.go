package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type AgentRunClaim struct {
	Scope                      Scope
	Kind                       RunKind
	WorkerID                   string
	AssignedAgentID            string
	Now                        time.Time
	LeaseDuration              time.Duration
	AgingInterval              time.Duration
	MaxActiveForAgent          int
	MaxActiveForOwner          int
	MaxActiveForObjective      int
	MaxActiveForConcurrencyKey int
}

func (c AgentRunClaim) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if c.Kind != "" && !validRunKind(c.Kind) {
		return fmt.Errorf("unsupported run kind %q", c.Kind)
	}
	if strings.TrimSpace(c.WorkerID) == "" || c.Now.IsZero() || c.LeaseDuration <= 0 || c.AgingInterval <= 0 {
		return errors.New("worker id, current time, lease duration, and aging interval are required")
	}
	if c.MaxActiveForAgent < 0 {
		return errors.New("max active runs cannot be negative")
	}
	if c.MaxActiveForOwner < 0 {
		return errors.New("max active runs per owner cannot be negative")
	}
	if c.MaxActiveForObjective < 0 {
		return errors.New("max active runs per objective cannot be negative")
	}
	if c.MaxActiveForConcurrencyKey < 0 {
		return errors.New("max active runs per concurrency key cannot be negative")
	}
	return nil
}

type AgentRunClaimRequest struct {
	Scope                      Scope
	Kind                       RunKind
	WorkerID                   string
	AssignedAgentID            string
	LeaseDuration              time.Duration
	AgingInterval              time.Duration
	MaxActiveForAgent          int
	MaxActiveForOwner          int
	MaxActiveForObjective      int
	MaxActiveForConcurrencyKey int
}

type AgentRunAdmissionOutcome string

const (
	AgentRunAdmissionClaimed       AgentRunAdmissionOutcome = "claimed"
	AgentRunAdmissionReady         AgentRunAdmissionOutcome = "ready"
	AgentRunAdmissionIdle          AgentRunAdmissionOutcome = "idle"
	AgentRunAdmissionNotDue        AgentRunAdmissionOutcome = "not_due"
	AgentRunAdmissionLeaseHeld     AgentRunAdmissionOutcome = "lease_held"
	AgentRunAdmissionBackpressured AgentRunAdmissionOutcome = "backpressured"
	AgentRunAdmissionBudgetStopped AgentRunAdmissionOutcome = "budget_stopped"
)

type AgentRunAdmissionReason string

const (
	AgentRunAdmissionReasonNotDue                 AgentRunAdmissionReason = "not_due"
	AgentRunAdmissionReasonLeaseHeld              AgentRunAdmissionReason = "lease_held"
	AgentRunAdmissionReasonAgentCapacity          AgentRunAdmissionReason = "agent_capacity"
	AgentRunAdmissionReasonOwnerCapacity          AgentRunAdmissionReason = "owner_capacity"
	AgentRunAdmissionReasonObjectiveCapacity      AgentRunAdmissionReason = "objective_capacity"
	AgentRunAdmissionReasonResourceCapacity       AgentRunAdmissionReason = "resource_capacity"
	AgentRunAdmissionReasonConcurrencyCapacity    AgentRunAdmissionReason = "concurrency_key_capacity"
	AgentRunAdmissionReasonAttemptBudgetExhausted AgentRunAdmissionReason = "attempt_budget_exhausted"
)

// AgentRunAdmissionBlock is safe operator-facing evidence for why one ready
// Run could not be admitted. It contains identifiers and counters, never Run
// context, prompts, Skill inputs, or credentials.
type AgentRunAdmissionBlock struct {
	RunID           string                  `json:"runId"`
	ObjectiveID     string                  `json:"objectiveId,omitempty"`
	AssignedAgentID string                  `json:"assignedAgentId,omitempty"`
	Owner           ObjectiveOwner          `json:"owner"`
	ConcurrencyKey  string                  `json:"concurrencyKey,omitempty"`
	Reason          AgentRunAdmissionReason `json:"reason"`
	Active          int                     `json:"active,omitempty"`
	Limit           int                     `json:"limit,omitempty"`
	Resource        string                  `json:"resource,omitempty"`
	Requested       int                     `json:"requested,omitempty"`
	Reserved        int                     `json:"reserved,omitempty"`
	Capacity        int                     `json:"capacity,omitempty"`
	ReadyAt         *time.Time              `json:"readyAt,omitempty"`
	LeaseExpiresAt  *time.Time              `json:"leaseExpiresAt,omitempty"`
}

type AgentRunAdmissionDecision struct {
	Outcome     AgentRunAdmissionOutcome `json:"outcome"`
	Run         *AgentRun                `json:"run,omitempty"`
	Blocks      []AgentRunAdmissionBlock `json:"blocks,omitempty"`
	NextWakeAt  *time.Time               `json:"nextWakeAt,omitempty"`
	EvaluatedAt time.Time                `json:"evaluatedAt"`
}

type AgentRunScheduleStore interface {
	ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error)
	RenewAgentRunLease(ctx context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error)
}

type AgentRunAdmissionStore interface {
	ClaimNextAgentRunWithDecision(ctx context.Context, claim AgentRunClaim) (*AgentRunAdmissionDecision, error)
}

type AgentRunScheduler struct {
	store AgentRunScheduleStore
	now   func() time.Time
}

func NewAgentRunScheduler(store AgentRunScheduleStore) *AgentRunScheduler {
	return &AgentRunScheduler{store: store, now: time.Now}
}

func (s *AgentRunScheduler) ClaimNext(ctx context.Context, req AgentRunClaimRequest) (*AgentRun, error) {
	decision, err := s.ClaimNextDecision(ctx, req)
	if err != nil || decision == nil {
		return nil, err
	}
	return decision.Run, nil
}

func (s *AgentRunScheduler) ClaimNextDecision(ctx context.Context, req AgentRunClaimRequest) (*AgentRunAdmissionDecision, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		return nil, errors.New("worker id is required")
	}
	if req.LeaseDuration <= 0 {
		req.LeaseDuration = 30 * time.Second
	}
	if req.AgingInterval <= 0 {
		req.AgingInterval = time.Minute
	}
	claim := AgentRunClaim{
		Scope: req.Scope, Kind: req.Kind, WorkerID: req.WorkerID, AssignedAgentID: req.AssignedAgentID,
		Now: s.now(), LeaseDuration: req.LeaseDuration, AgingInterval: req.AgingInterval,
		MaxActiveForAgent: req.MaxActiveForAgent, MaxActiveForOwner: req.MaxActiveForOwner,
		MaxActiveForObjective: req.MaxActiveForObjective, MaxActiveForConcurrencyKey: req.MaxActiveForConcurrencyKey,
	}
	if store, ok := s.store.(AgentRunAdmissionStore); ok {
		return store.ClaimNextAgentRunWithDecision(ctx, claim)
	}
	run, err := s.store.ClaimNextAgentRun(ctx, claim)
	if err != nil {
		return nil, err
	}
	outcome := AgentRunAdmissionIdle
	if run != nil {
		outcome = AgentRunAdmissionClaimed
	}
	return &AgentRunAdmissionDecision{Outcome: outcome, Run: run, EvaluatedAt: claim.Now}, nil
}

// Inspect returns the current portfolio admission decision without acquiring a
// lease. It is an observational projection for operators and user interfaces;
// only ClaimNextDecision grants execution authority.
func (s *AgentRunScheduler) Inspect(ctx context.Context, req AgentRunClaimRequest) (*AgentRunAdmissionDecision, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	portfolio, ok := s.store.(PortfolioStore)
	if !ok {
		return nil, errors.New("agent run admission inspection is unavailable")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.AgingInterval <= 0 {
		req.AgingInterval = time.Minute
	}
	claim := AgentRunClaim{
		Scope: req.Scope, Kind: req.Kind, WorkerID: "admission-inspector", Now: s.now(), LeaseDuration: time.Second,
		AgingInterval: req.AgingInterval, AssignedAgentID: req.AssignedAgentID,
		MaxActiveForAgent: req.MaxActiveForAgent, MaxActiveForOwner: req.MaxActiveForOwner,
		MaxActiveForObjective: req.MaxActiveForObjective, MaxActiveForConcurrencyKey: req.MaxActiveForConcurrencyKey,
	}
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	runs := make([]*AgentRun, 0)
	for offset := 0; ; offset += 500 {
		page, err := portfolio.ListAgentRuns(ctx, AgentRunFilter{
			Scope: req.Scope, Kind: req.Kind, AssignedAgentID: req.AssignedAgentID,
			Statuses: []AgentRunStatus{AgentRunStatusQueued, AgentRunStatusRunning}, Limit: 500, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		runs = append(runs, page...)
		if len(page) < 500 {
			break
		}
	}
	objectives := make(map[string]*Objective)
	for offset := 0; ; offset += 500 {
		page, err := portfolio.ListObjectives(ctx, ObjectiveFilter{Scope: req.Scope, IncludeRetired: true, Limit: 500, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, objective := range page {
			objectives[objective.ID] = objective
		}
		if len(page) < 500 {
			break
		}
	}
	selected, decision := evaluateAgentRunAdmission(runs, objectives, claim)
	if selected != nil {
		decision.Run = cloneAgentRun(selected)
	}
	return decision, nil
}

func agentRunOwnerSchedulingKey(owner ObjectiveOwner) string {
	return string(owner.Type) + "\x1f" + owner.ID
}

func effectiveObjectiveConcurrencyLimit(runtimeLimit int, objective *Objective) int {
	objectiveLimit := 0
	if objective != nil && objective.ExecutionPolicy != nil {
		objectiveLimit = objective.ExecutionPolicy.MaximumConcurrentRuns
	}
	switch {
	case runtimeLimit > 0 && objectiveLimit > 0 && runtimeLimit < objectiveLimit:
		return runtimeLimit
	case objectiveLimit > 0:
		return objectiveLimit
	default:
		return runtimeLimit
	}
}

func evaluateAgentRunAdmission(runs []*AgentRun, objectives map[string]*Objective, claim AgentRunClaim) (*AgentRun, *AgentRunAdmissionDecision) {
	decision := &AgentRunAdmissionDecision{Outcome: AgentRunAdmissionIdle, EvaluatedAt: claim.Now}
	activeByAgent := make(map[string]int)
	activeByOwner := make(map[string]int)
	activeByObjective := make(map[string]int)
	activeByConcurrencyKey := make(map[string]int)
	reservedByObjectiveResource := make(map[string]int)
	resourceWakeByObjectiveResource := make(map[string]*time.Time)
	objectiveWake := make(map[string]*time.Time)
	for _, run := range runs {
		if run == nil || run.Scope != claim.Scope || run.Status != AgentRunStatusRunning || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(claim.Now) {
			continue
		}
		activeByAgent[run.AssignedAgentID]++
		activeByOwner[agentRunOwnerSchedulingKey(run.Owner)]++
		activeByObjective[run.ObjectiveID]++
		activeByConcurrencyKey[run.ConcurrencyKey]++
		objectiveWake[run.ObjectiveID] = earliestTime(objectiveWake[run.ObjectiveID], run.LeaseExpiresAt)
		for resource, quantity := range run.ResourceRequirements {
			key := objectiveResourceSchedulingKey(run.ObjectiveID, resource)
			reservedByObjectiveResource[key] += quantity
			resourceWakeByObjectiveResource[key] = earliestTime(resourceWakeByObjectiveResource[key], run.LeaseExpiresAt)
		}
	}
	var selected *AgentRun
	for _, run := range runs {
		if run == nil || run.Scope != claim.Scope || claim.Kind != "" && normalizeRunKind(run.Kind) != claim.Kind ||
			claim.AssignedAgentID != "" && run.AssignedAgentID != claim.AssignedAgentID {
			continue
		}
		base := AgentRunAdmissionBlock{RunID: run.ID, ObjectiveID: run.ObjectiveID, AssignedAgentID: run.AssignedAgentID, Owner: run.Owner, ConcurrencyKey: run.ConcurrencyKey}
		if run.Status == AgentRunStatusQueued && run.AvailableAt.After(claim.Now) {
			block := base
			block.Reason, block.ReadyAt = AgentRunAdmissionReasonNotDue, cloneAdmissionTime(&run.AvailableAt)
			decision.Blocks = append(decision.Blocks, block)
			decision.NextWakeAt = earliestTime(decision.NextWakeAt, &run.AvailableAt)
			continue
		}
		if run.Status == AgentRunStatusRunning && run.LeaseExpiresAt != nil && run.LeaseExpiresAt.After(claim.Now) {
			block := base
			block.Reason, block.LeaseExpiresAt = AgentRunAdmissionReasonLeaseHeld, cloneAdmissionTime(run.LeaseExpiresAt)
			decision.Blocks = append(decision.Blocks, block)
			decision.NextWakeAt = earliestTime(decision.NextWakeAt, run.LeaseExpiresAt)
			continue
		}
		if !agentRunEligible(run, claim) {
			continue
		}
		if claim.MaxActiveForAgent > 0 && run.AssignedAgentID != "" && activeByAgent[run.AssignedAgentID] >= claim.MaxActiveForAgent {
			block := base
			block.Reason, block.Active, block.Limit = AgentRunAdmissionReasonAgentCapacity, activeByAgent[run.AssignedAgentID], claim.MaxActiveForAgent
			decision.Blocks = append(decision.Blocks, block)
			continue
		}
		if claim.MaxActiveForOwner > 0 && activeByOwner[agentRunOwnerSchedulingKey(run.Owner)] >= claim.MaxActiveForOwner {
			block := base
			block.Reason, block.Active, block.Limit = AgentRunAdmissionReasonOwnerCapacity, activeByOwner[agentRunOwnerSchedulingKey(run.Owner)], claim.MaxActiveForOwner
			decision.Blocks = append(decision.Blocks, block)
			continue
		}
		objectiveLimit := effectiveObjectiveConcurrencyLimit(claim.MaxActiveForObjective, objectives[run.ObjectiveID])
		if objectiveLimit > 0 && run.ObjectiveID != "" && activeByObjective[run.ObjectiveID] >= objectiveLimit {
			block := base
			block.Reason, block.Active, block.Limit = AgentRunAdmissionReasonObjectiveCapacity, activeByObjective[run.ObjectiveID], objectiveLimit
			decision.Blocks = append(decision.Blocks, block)
			decision.NextWakeAt = earliestTime(decision.NextWakeAt, objectiveWake[run.ObjectiveID])
			continue
		}
		objective := objectives[run.ObjectiveID]
		resourceNames := make([]string, 0, len(run.ResourceRequirements))
		for resource := range run.ResourceRequirements {
			resourceNames = append(resourceNames, resource)
		}
		sort.Strings(resourceNames)
		resourceBlocked := false
		for _, resource := range resourceNames {
			if objective == nil || objective.ExecutionPolicy == nil {
				continue
			}
			capacity, bounded := objective.ExecutionPolicy.ResourceCapacities[resource]
			if !bounded {
				continue
			}
			key := objectiveResourceSchedulingKey(run.ObjectiveID, resource)
			requested, reserved := run.ResourceRequirements[resource], reservedByObjectiveResource[key]
			if reserved+requested <= capacity {
				continue
			}
			block := base
			block.Reason, block.Resource = AgentRunAdmissionReasonResourceCapacity, resource
			block.Requested, block.Reserved, block.Capacity = requested, reserved, capacity
			decision.Blocks = append(decision.Blocks, block)
			decision.NextWakeAt = earliestTime(decision.NextWakeAt, resourceWakeByObjectiveResource[key])
			resourceBlocked = true
		}
		if resourceBlocked {
			continue
		}
		if claim.MaxActiveForConcurrencyKey > 0 && run.ConcurrencyKey != "" && activeByConcurrencyKey[run.ConcurrencyKey] >= claim.MaxActiveForConcurrencyKey {
			block := base
			block.Reason, block.Active, block.Limit = AgentRunAdmissionReasonConcurrencyCapacity, activeByConcurrencyKey[run.ConcurrencyKey], claim.MaxActiveForConcurrencyKey
			decision.Blocks = append(decision.Blocks, block)
			continue
		}
		if selected == nil || agentRunSchedulesBefore(run, selected, claim.Now, claim.AgingInterval) {
			selected = run
		}
	}
	if selected != nil {
		decision.Outcome = AgentRunAdmissionReady
		return selected, decision
	}
	hasCapacity, hasNotDue, hasLease := false, false, false
	for _, block := range decision.Blocks {
		switch block.Reason {
		case AgentRunAdmissionReasonAgentCapacity, AgentRunAdmissionReasonOwnerCapacity, AgentRunAdmissionReasonObjectiveCapacity, AgentRunAdmissionReasonResourceCapacity, AgentRunAdmissionReasonConcurrencyCapacity:
			hasCapacity = true
		case AgentRunAdmissionReasonNotDue:
			hasNotDue = true
		case AgentRunAdmissionReasonLeaseHeld:
			hasLease = true
		}
	}
	switch {
	case hasCapacity:
		decision.Outcome = AgentRunAdmissionBackpressured
	case hasNotDue:
		decision.Outcome = AgentRunAdmissionNotDue
	case hasLease:
		decision.Outcome = AgentRunAdmissionLeaseHeld
	}
	return nil, decision
}

func objectiveResourceSchedulingKey(objectiveID, resource string) string {
	return objectiveID + "\x1f" + resource
}

func earliestTime(current, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.Before(*current) {
		return cloneAdmissionTime(candidate)
	}
	return current
}

func cloneAdmissionTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (s *AgentRunScheduler) RenewLease(ctx context.Context, scope Scope, runID, workerID string, leaseDuration time.Duration) (*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("agent run scheduler is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(workerID) == "" {
		return nil, errors.New("run id and worker id are required")
	}
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}
	return s.store.RenewAgentRunLease(ctx, scope, runID, workerID, s.now(), leaseDuration)
}

func agentRunEligible(run *AgentRun, claim AgentRunClaim) bool {
	if run.Scope != claim.Scope || claim.Kind != "" && normalizeRunKind(run.Kind) != claim.Kind ||
		claim.AssignedAgentID != "" && run.AssignedAgentID != claim.AssignedAgentID {
		return false
	}
	switch run.Status {
	case AgentRunStatusQueued:
		return !run.AvailableAt.After(claim.Now) && (run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(claim.Now))
	case AgentRunStatusRunning:
		return run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(claim.Now)
	default:
		return false
	}
}

// applyAgentRunClaim records the durable cost of acquiring autonomous
// execution authority. A lease-expiry reclaim is a new attempt even when it
// resumes an existing Turn, so process restarts cannot reset this ceiling.
func applyAgentRunClaim(run *AgentRun, claim AgentRunClaim) error {
	if runAttemptBudgetAtLimit(run) {
		run.PausedFrom = run.Status
		run.Status = AgentRunStatusPaused
		run.LeaseOwner = ""
		run.LeaseExpiresAt = nil
		run.BudgetState = BudgetStateExhausted
		run.Revision++
		run.UpdatedAt = claim.Now
		return nil
	}
	expires := claim.Now.Add(claim.LeaseDuration)
	run.Status = AgentRunStatusRunning
	run.LeaseOwner = claim.WorkerID
	run.LeaseExpiresAt = &expires
	run.LastClaimedAt = &claim.Now
	run.Attempt++
	if run.Budget != nil {
		usage, err := run.BudgetUsage.Add(BudgetUsage{Attempts: 1})
		if err != nil {
			return err
		}
		effective, err := EffectiveBudgetUsage(usage, run.BudgetReservations)
		if err != nil {
			return err
		}
		state, _, err := EvaluateBudget(*run.Budget, effective)
		if err != nil {
			return err
		}
		run.BudgetUsage = usage
		run.BudgetState = state
	}
	run.Revision++
	run.UpdatedAt = claim.Now
	if run.StartedAt == nil {
		run.StartedAt = &claim.Now
	}
	return nil
}

func runAttemptBudgetExceeded(run *AgentRun) bool {
	return run != nil && run.Budget != nil && run.Budget.MaxAttempts > 0 && run.BudgetUsage.Attempts > run.Budget.MaxAttempts
}

func runAttemptBudgetAtLimit(run *AgentRun) bool {
	return run != nil && run.Budget != nil && run.Budget.MaxAttempts > 0 && run.BudgetUsage.Attempts >= run.Budget.MaxAttempts
}

func agentRunEffectivePriority(run *AgentRun, now time.Time, agingInterval time.Duration) int64 {
	if agingInterval <= 0 {
		agingInterval = time.Minute
	}
	entered := run.QueueEnteredAt
	if entered.IsZero() {
		entered = run.CreatedAt
	}
	age := now.Sub(entered)
	if age < 0 {
		age = 0
	}
	return int64(run.Priority) + int64(age/agingInterval)
}

func agentRunSchedulesBefore(left, right *AgentRun, now time.Time, agingInterval time.Duration) bool {
	leftScore := agentRunEffectivePriority(left, now, agingInterval)
	rightScore := agentRunEffectivePriority(right, now, agingInterval)
	if leftScore != rightScore {
		return leftScore > rightScore
	}
	if left.Deadline != nil || right.Deadline != nil {
		if left.Deadline == nil {
			return false
		}
		if right.Deadline == nil {
			return true
		}
		if !left.Deadline.Equal(*right.Deadline) {
			return left.Deadline.Before(*right.Deadline)
		}
	}
	if !left.QueueEnteredAt.Equal(right.QueueEnteredAt) {
		return left.QueueEnteredAt.Before(right.QueueEnteredAt)
	}
	return left.ID < right.ID
}
