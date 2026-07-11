package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrInvalidRunTransition = errors.New("invalid run transition")

type ActivitySeverity string

const (
	ActivitySeverityDebug   ActivitySeverity = "debug"
	ActivitySeverityInfo    ActivitySeverity = "info"
	ActivitySeverityWarning ActivitySeverity = "warning"
	ActivitySeverityError   ActivitySeverity = "error"
)

type ActivityVisibility string

const (
	ActivityVisibilityPrivate ActivityVisibility = "private"
	ActivityVisibilityTeam    ActivityVisibility = "team"
	ActivityVisibilityScope   ActivityVisibility = "scope"
)

type ActivityActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func teamIDForRun(run *AgentRun) string {
	if run != nil && run.Owner.Type == OwnerTypeTeam {
		return run.Owner.ID
	}
	return ""
}

// ActivityEvent is the append-only operator-facing record of meaningful work.
// It stores concise rationale and evidence references, never private model
// chain-of-thought or resolved secret values.
type ActivityEvent struct {
	ID               string                 `json:"id"`
	Sequence         int64                  `json:"sequence"`
	Scope            Scope                  `json:"scope"`
	EventType        string                 `json:"eventType"`
	Severity         ActivitySeverity       `json:"severity"`
	AgentID          string                 `json:"agentId,omitempty"`
	ObjectiveID      string                 `json:"objectiveId,omitempty"`
	RunID            string                 `json:"runId"`
	TurnID           string                 `json:"turnId,omitempty"`
	ParentRunID      string                 `json:"parentRunId,omitempty"`
	TeamID           string                 `json:"teamId,omitempty"`
	ConversationRefs []string               `json:"conversationRefs,omitempty"`
	Actor            ActivityActor          `json:"actor"`
	Summary          string                 `json:"summary"`
	Payload          map[string]interface{} `json:"payload,omitempty"`
	Visibility       ActivityVisibility     `json:"visibility"`
	CorrelationID    string                 `json:"correlationId,omitempty"`
	CausationID      string                 `json:"causationId,omitempty"`
	CreatedAt        time.Time              `json:"createdAt"`
}

func (e *ActivityEvent) Validate() error {
	if e == nil {
		return errors.New("activity event is required")
	}
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(e.RunID) == "" || strings.TrimSpace(e.EventType) == "" || strings.TrimSpace(e.Summary) == "" {
		return errors.New("activity run, type, and summary are required")
	}
	return nil
}

type RunTransitionRequest struct {
	ExpectedRevision int64
	Status           AgentRunStatus
	Summary          string
	Actor            ActivityActor
	Severity         ActivitySeverity
	Visibility       ActivityVisibility
	Payload          map[string]interface{}
	Plan             map[string]interface{}
	Checkpoint       map[string]interface{}
	WakeCondition    *WakeCondition
	Output           map[string]interface{}
	Error            string
	CorrelationID    string
	CausationID      string
	TurnID           string
	AppliedTurn      int64
	LeaseOwner       string
	WakeSignalID     string
	EventType        string
	OccurredAt       *time.Time
	Intervention     *AgentRunIntervention
	BudgetUsageDelta *BudgetUsage
}

type AgentRunLeaseGuard struct {
	WorkerID string
	Now      time.Time
}

type ActivityFilter struct {
	Scope           Scope
	RunID           string
	AgentID         string
	ObjectiveID     string
	TeamID          string
	EventTypes      []string
	Severities      []ActivitySeverity
	Visibilities    []ActivityVisibility
	AfterSequence   int64
	BeforeCreatedAt *time.Time
	BeforeID        string
	Descending      bool
	Limit           int
}

type RunActivityStore interface {
	CreateAgentRunWithEvent(ctx context.Context, run *AgentRun, event *ActivityEvent) (*ActivityEvent, error)
	UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error)
	AppendActivity(ctx context.Context, event *ActivityEvent) (*ActivityEvent, error)
	ListActivity(ctx context.Context, filter ActivityFilter) ([]*ActivityEvent, error)
}

type RunActivityService struct {
	portfolio PortfolioStore
	activity  RunActivityStore
	now       func() time.Time
}

func NewRunActivityService(portfolio PortfolioStore, activity RunActivityStore) *RunActivityService {
	return &RunActivityService{portfolio: portfolio, activity: activity, now: time.Now}
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
	if req.Output != nil {
		run.Output = req.Output
	}
	run.Error = req.Error
	if req.WakeSignalID != "" {
		run.LastWakeSignalID = req.WakeSignalID
	}
	if req.AppliedTurn > 0 {
		if req.AppliedTurn != run.LastAppliedTurn+1 {
			return nil, nil, fmt.Errorf("%w: applied turn %d after %d", ErrRevisionConflict, req.AppliedTurn, run.LastAppliedTurn)
		}
		run.LastAppliedTurn = req.AppliedTurn
	}
	if req.BudgetUsageDelta != nil {
		if run.BudgetPolicy == nil {
			return nil, nil, errors.New("budget usage cannot be recorded without a budget policy")
		}
		usage, err := run.BudgetUsage.Add(*req.BudgetUsageDelta)
		if err != nil {
			return nil, nil, err
		}
		state, _, err := EvaluateBudget(*run.BudgetPolicy, usage)
		if err != nil {
			return nil, nil, err
		}
		run.BudgetUsage = usage
		run.BudgetState = state
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
		TurnID: req.TurnID,
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

func canTransitionAgentRun(from, to AgentRunStatus) bool {
	allowed := map[AgentRunStatus]map[AgentRunStatus]bool{
		AgentRunStatusQueued:               {AgentRunStatusPlanning: true, AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusCanceled: true},
		AgentRunStatusPlanning:             {AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusWaitingForAgent: true, AgentRunStatusWaitingForApproval: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusRunning:              {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusSleeping: true, AgentRunStatusWaitingForDependency: true, AgentRunStatusWaitingForAgent: true, AgentRunStatusWaitingForApproval: true, AgentRunStatusWaitingForEvent: true, AgentRunStatusCompleted: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusPaused:               {AgentRunStatusQueued: true, AgentRunStatusSleeping: true, AgentRunStatusWaitingForDependency: true, AgentRunStatusWaitingForAgent: true, AgentRunStatusWaitingForApproval: true, AgentRunStatusWaitingForEvent: true, AgentRunStatusCanceled: true},
		AgentRunStatusSleeping:             {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForDependency: {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForAgent:      {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForApproval:   {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForEvent:      {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusPaused: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
	}
	return allowed[from][to]
}

func isWaitingRunStatus(status AgentRunStatus) bool {
	return status == AgentRunStatusSleeping || status == AgentRunStatusWaitingForDependency ||
		status == AgentRunStatusWaitingForAgent || status == AgentRunStatusWaitingForApproval ||
		status == AgentRunStatusWaitingForEvent
}

func isTerminalAgentRunStatus(status AgentRunStatus) bool {
	return status == AgentRunStatusCompleted || status == AgentRunStatusFailed || status == AgentRunStatusCanceled
}
