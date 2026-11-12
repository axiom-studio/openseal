package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

type RunTransitionRequest struct {
	ExpectedRevision          int64
	Status                    AgentRunStatus
	Summary                   string
	Actor                     ActivityActor
	Severity                  ActivitySeverity
	Visibility                ActivityVisibility
	Payload                   map[string]interface{}
	Plan                      map[string]interface{}
	Checkpoint                map[string]interface{}
	WakeCondition             *WakeCondition
	AvailableAt               *time.Time // delays a queued Run's next claim
	Output                    map[string]interface{}
	Error                     string
	CorrelationID             string
	CausationID               string
	TurnID                    string
	AppliedTurn               int64
	LeaseOwner                string
	WakeSignalID              string
	EventType                 string
	OccurredAt                *time.Time
	Intervention              *AgentRunIntervention
	HumanInterventions        []HumanInterventionRequest
	BudgetUsageDelta          *BudgetUsage
	ActivityUsageDelta        *BudgetUsage
	BudgetAdmission           *BudgetAdmission
	BudgetReservation         *BudgetReservation
	ReplaceBudgetReservation  *BudgetReservation
	SettleBudgetReservationID string
	// RequireInterventionCount, when non-nil, fails the transition with
	// ErrSteeringArrived unless len(PendingInterventions) equals it. The
	// revision CAS makes the check atomic: every intervention append bumps
	// the Run's revision.
	RequireInterventionCount *int
	// FailureExplanation replaces AgentRun.FailureExplanation when non-nil.
	FailureExplanation *RunFailureExplanation
}

type RunActivityService struct {
	portfolio PortfolioStore
	activity  RunActivityStore
	now       func() time.Time
}

func NewRunActivityService(portfolio PortfolioStore, activity RunActivityStore, opts ...models.ServiceOption) *RunActivityService {
	return &RunActivityService{portfolio: portfolio, activity: activity, now: models.ClockFrom(opts)}
}

func (s *RunActivityService) TransitionRun(ctx context.Context, scope Scope, runID string, req RunTransitionRequest) (*AgentRun, *ActivityEvent, error) {
	if s == nil || s.portfolio == nil || s.activity == nil {
		return nil, nil, errors.New("run activity service is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, nil, err
	}
	run, err := s.portfolio.GetAgentRun(ctx, scope, runID)
	if err != nil {
		return nil, nil, err
	}
	if run == nil {
		return nil, nil, ErrRunNotFound
	}
	if req.ExpectedRevision != run.Revision {
		return nil, nil, ErrRevisionConflict
	}
	var leaseGuard *AgentRunLeaseGuard
	if req.LeaseOwner != "" {
		now := s.now()
		if run.LeaseOwner != req.LeaseOwner || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
			return nil, nil, ErrLeaseLost
		}
		leaseGuard = &AgentRunLeaseGuard{WorkerID: req.LeaseOwner, Now: now}
	}
	if req.RequireInterventionCount != nil && len(run.PendingInterventions) != *req.RequireInterventionCount {
		return nil, nil, ErrSteeringArrived
	}
	if !canTransitionAgentRun(run.Status, req.Status) && !(req.Intervention != nil && run.Status == req.Status && !isTerminalAgentRunStatus(run.Status)) {
		return nil, nil, fmt.Errorf("%w: %s -> %s", ErrInvalidRunTransition, run.Status, req.Status)
	}
	previousStatus := run.Status
	if isWaitingRunStatus(req.Status) && req.WakeCondition == nil {
		return nil, nil, fmt.Errorf("%w: %s requires a wake condition", ErrInvalidRunTransition, req.Status)
	}
	now := s.now()
	if req.OccurredAt != nil {
		now = *req.OccurredAt
	}
	previousWakeCondition := run.WakeCondition
	run.Status = req.Status
	run.Revision++
	run.UpdatedAt = now
	if req.Plan != nil {
		run.Plan = req.Plan
	}
	if req.Checkpoint != nil {
		run.Checkpoint = req.Checkpoint
	}
	run.WakeCondition = req.WakeCondition
	if req.Status == AgentRunStatusPaused && previousStatus != AgentRunStatusPaused {
		run.PausedFrom = previousStatus
		run.PausedWakeCondition = previousWakeCondition
		run.WakeCondition = nil
	} else if previousStatus == AgentRunStatusPaused && req.Status != AgentRunStatusPaused {
		run.PausedFrom = ""
		run.PausedWakeCondition = nil
	}
	if req.Intervention != nil {
		run.PendingInterventions = append(run.PendingInterventions, *req.Intervention)
	}
	if req.HumanInterventions != nil {
		run.HumanInterventions = append([]HumanInterventionRequest(nil), req.HumanInterventions...)
	}
	if req.Output != nil {
		run.Output = req.Output
	}
	run.Error = req.Error
	if req.FailureExplanation != nil {
		explanation := *req.FailureExplanation
		run.FailureExplanation = &explanation
	}
	run.CreditError = terminalCreditError(run.Status, run.FailureExplanation)
	if req.WakeSignalID != "" {
		run.LastWakeSignalID = req.WakeSignalID
	}
	if req.AppliedTurn > 0 {
		if req.AppliedTurn != run.LastAppliedTurn+1 {
			return nil, nil, fmt.Errorf("%w: applied turn %d after %d", ErrRevisionConflict, req.AppliedTurn, run.LastAppliedTurn)
		}
		run.LastAppliedTurn = req.AppliedTurn
	}
	if req.BudgetReservation != nil {
		if run.Budget == nil {
			return nil, nil, errors.New("budget cannot be reserved without a budget policy")
		}
		if err := req.BudgetReservation.Validate(); err != nil {
			return nil, nil, err
		}
		if run.BudgetReservations == nil {
			run.BudgetReservations = make(map[string]BudgetReservation)
		}
		if _, exists := run.BudgetReservations[req.BudgetReservation.ID]; exists {
			return nil, nil, errors.New("budget reservation already exists")
		}
		run.BudgetReservations[req.BudgetReservation.ID] = *req.BudgetReservation
		effective, err := EffectiveBudgetUsage(run.BudgetUsage, run.BudgetReservations)
		if err != nil {
			return nil, nil, err
		}
		exceeded, _, err := BudgetWouldExceed(*run.Budget, effective)
		if err != nil {
			return nil, nil, err
		}
		if exceeded {
			return nil, nil, ErrBudgetExhausted
		}
		state, _, err := EvaluateBudget(*run.Budget, effective)
		if err != nil {
			return nil, nil, err
		}
		run.BudgetState = state
		run.BudgetAdmission = nil
	}
	if req.ReplaceBudgetReservation != nil {
		if run.Budget == nil {
			return nil, nil, errors.New("budget reservation cannot be replaced without a budget policy")
		}
		if req.BudgetReservation != nil {
			return nil, nil, errors.New("budget reservation cannot be created and replaced in one transition")
		}
		if err := req.ReplaceBudgetReservation.Validate(); err != nil {
			return nil, nil, err
		}
		if _, exists := run.BudgetReservations[req.ReplaceBudgetReservation.ID]; !exists {
			return nil, nil, errors.New("budget reservation does not exist")
		}
		run.BudgetReservations[req.ReplaceBudgetReservation.ID] = *req.ReplaceBudgetReservation
		effective, err := EffectiveBudgetUsage(run.BudgetUsage, run.BudgetReservations)
		if err != nil {
			return nil, nil, err
		}
		exceeded, _, err := BudgetWouldExceed(*run.Budget, effective)
		if err != nil {
			return nil, nil, err
		}
		if exceeded {
			return nil, nil, ErrBudgetExhausted
		}
		state, _, err := EvaluateBudget(*run.Budget, effective)
		if err != nil {
			return nil, nil, err
		}
		run.BudgetState = state
		run.BudgetAdmission = nil
	}
	if req.SettleBudgetReservationID != "" {
		if _, exists := run.BudgetReservations[req.SettleBudgetReservationID]; !exists {
			return nil, nil, errors.New("budget reservation does not exist")
		}
		delete(run.BudgetReservations, req.SettleBudgetReservationID)
	}
	if req.BudgetUsageDelta != nil {
		if run.Budget == nil {
			return nil, nil, errors.New("budget usage cannot be recorded without a budget policy")
		}
		usage, err := run.BudgetUsage.Add(*req.BudgetUsageDelta)
		if err != nil {
			return nil, nil, err
		}
		effective, err := EffectiveBudgetUsage(usage, run.BudgetReservations)
		if err != nil {
			return nil, nil, err
		}
		state, _, err := EvaluateBudget(*run.Budget, effective)
		if err != nil {
			return nil, nil, err
		}
		run.BudgetUsage = usage
		run.BudgetState = state
		run.BudgetAdmission = nil
	}
	if req.BudgetAdmission != nil {
		if run.Budget == nil {
			return nil, nil, errors.New("budget admission cannot be recorded without a budget policy")
		}
		if err := req.BudgetAdmission.Validate(); err != nil {
			return nil, nil, err
		}
		admission := *req.BudgetAdmission
		run.BudgetAdmission = &admission
		run.BudgetState = BudgetStateExhausted
	}
	if run.StartedAt == nil && (req.Status == AgentRunStatusPlanning || req.Status == AgentRunStatusRunning) {
		run.StartedAt = &now
	}
	if isTerminalAgentRunStatus(req.Status) {
		run.CompletedAt = &now
		run.WakeCondition = nil
	}
	if isWaitingRunStatus(req.Status) || isTerminalAgentRunStatus(req.Status) || req.Status == AgentRunStatusQueued || req.Status == AgentRunStatusPaused {
		run.LeaseOwner = ""
		run.LeaseExpiresAt = nil
	}
	if req.Status == AgentRunStatusQueued {
		run.AvailableAt = now
		if req.AvailableAt != nil && req.AvailableAt.After(now) {
			run.AvailableAt = *req.AvailableAt
		}
		run.QueueEnteredAt = now
	}
	severity := req.Severity
	if severity == "" {
		severity = ActivitySeverityInfo
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	eventType := req.EventType
	if eventType == "" {
		eventType = "run.transitioned"
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: scope, EventType: eventType, Severity: severity,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, TeamID: teamIDForRun(run),
		ParentRunID: run.ParentRunID, Actor: req.Actor, Summary: req.Summary,
		Payload: req.Payload, Visibility: visibility, CorrelationID: req.CorrelationID,
		CausationID: req.CausationID, CreatedAt: now,
		TurnID: req.TurnID, UsageDelta: cloneBudgetUsage(req.ActivityUsageDelta),
	}
	if strings.TrimSpace(event.Summary) == "" {
		event.Summary = fmt.Sprintf("Run moved from %s to %s", previousStatus, req.Status)
	}
	if err := event.Validate(); err != nil {
		return nil, nil, err
	}
	persisted, err := s.activity.UpdateAgentRunWithEvent(ctx, run, req.ExpectedRevision, event, leaseGuard)
	if err != nil {
		return nil, nil, err
	}
	return run, persisted, nil
}

func (s *RunActivityService) AppendActivity(ctx context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if s == nil || s.activity == nil {
		return nil, errors.New("run activity service is not configured")
	}
	if event == nil {
		return nil, errors.New("activity event is required")
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = s.now()
	}
	if event.Severity == "" {
		event.Severity = ActivitySeverityInfo
	}
	if event.Visibility == "" {
		event.Visibility = ActivityVisibilityScope
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return s.activity.AppendActivity(ctx, event)
}

func (s *RunActivityService) ListActivity(ctx context.Context, filter ActivityFilter) ([]*ActivityEvent, error) {
	if s == nil || s.activity == nil {
		return nil, errors.New("run activity service is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(filter.RunID) == "" {
		return nil, errors.New("run id is required")
	}
	filter.Descending = false
	return s.activity.ListActivity(ctx, filter)
}
