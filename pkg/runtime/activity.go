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
}

type ActivityFilter struct {
	Scope         Scope
	RunID         string
	AfterSequence int64
	Limit         int
}

type RunActivityStore interface {
	UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent) (*ActivityEvent, error)
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
	if !canTransitionAgentRun(run.Status, req.Status) {
		return nil, nil, fmt.Errorf("%w: %s -> %s", ErrInvalidRunTransition, run.Status, req.Status)
	}
	previousStatus := run.Status
	if isWaitingRunStatus(req.Status) && req.WakeCondition == nil {
		return nil, nil, fmt.Errorf("%w: %s requires a wake condition", ErrInvalidRunTransition, req.Status)
	}
	now := s.now()
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
	if req.Output != nil {
		run.Output = req.Output
	}
	run.Error = req.Error
	if req.AppliedTurn > 0 {
		if req.AppliedTurn != run.LastAppliedTurn+1 {
			return nil, nil, fmt.Errorf("%w: applied turn %d after %d", ErrRevisionConflict, req.AppliedTurn, run.LastAppliedTurn)
		}
		run.LastAppliedTurn = req.AppliedTurn
	}
	if run.StartedAt == nil && (req.Status == AgentRunStatusPlanning || req.Status == AgentRunStatusRunning) {
		run.StartedAt = &now
	}
	if isTerminalAgentRunStatus(req.Status) {
		run.CompletedAt = &now
		run.WakeCondition = nil
	}
	severity := req.Severity
	if severity == "" {
		severity = ActivitySeverityInfo
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: scope, EventType: "run.transitioned", Severity: severity,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID,
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
	persisted, err := s.activity.UpdateAgentRunWithEvent(ctx, run, req.ExpectedRevision, event)
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
	return s.activity.ListActivity(ctx, filter)
}

func canTransitionAgentRun(from, to AgentRunStatus) bool {
	allowed := map[AgentRunStatus]map[AgentRunStatus]bool{
		AgentRunStatusQueued:               {AgentRunStatusPlanning: true, AgentRunStatusRunning: true, AgentRunStatusCanceled: true},
		AgentRunStatusPlanning:             {AgentRunStatusRunning: true, AgentRunStatusWaitingForAgent: true, AgentRunStatusWaitingForApproval: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusRunning:              {AgentRunStatusSleeping: true, AgentRunStatusWaitingForDependency: true, AgentRunStatusWaitingForAgent: true, AgentRunStatusWaitingForApproval: true, AgentRunStatusWaitingForEvent: true, AgentRunStatusCompleted: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusSleeping:             {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForDependency: {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForAgent:      {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForApproval:   {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
		AgentRunStatusWaitingForEvent:      {AgentRunStatusQueued: true, AgentRunStatusRunning: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true},
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
