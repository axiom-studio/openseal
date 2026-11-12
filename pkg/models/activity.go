package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrInvalidRunTransition = errors.New("invalid run transition")

// ErrSteeringArrived reports that a guarded transition saw interventions the
// Run's loop has not read yet; nothing was written.
var ErrSteeringArrived = errors.New("new guidance arrived before the final answer was posted")

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

func TeamIDForRun(run *AgentRun) string {
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
	UsageDelta       *BudgetUsage           `json:"usageDelta,omitempty"`
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
	if (strings.TrimSpace(e.RunID) == "" && strings.TrimSpace(e.ObjectiveID) == "" && activityConversationSubject(e) == "") || strings.TrimSpace(e.EventType) == "" || strings.TrimSpace(e.Summary) == "" {
		return errors.New("activity subject, type, and summary are required")
	}
	if e.UsageDelta != nil {
		if err := e.UsageDelta.Validate(); err != nil {
			return fmt.Errorf("activity usage delta: %w", err)
		}
	}
	return nil
}

type AgentRunLeaseGuard struct {
	WorkerID string
	Now      time.Time
}

type ActivityFilter struct {
	Scope           Scope
	RunID           string
	RunIDs          []string
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

type ObjectiveActivityStore interface {
	CreateObjectiveWithEvent(ctx context.Context, objective *Objective, event *ActivityEvent) (*ActivityEvent, error)
	UpdateObjectiveWithEvent(ctx context.Context, objective *Objective, expectedRevision int64, event *ActivityEvent) (*ActivityEvent, error)
}

func ActivityStreamID(event *ActivityEvent) string {
	if event == nil {
		return ""
	}
	if event.RunID != "" {
		return event.RunID
	}
	if event.ObjectiveID == "" {
		if conversationID := activityConversationSubject(event); conversationID != "" {
			return "conversation:" + conversationID
		}
	}
	return "objective:" + event.ObjectiveID
}

// activityConversationSubject returns the conversation a conversation-level
// event (one without a Run or Objective) is recorded against. Such
// events are committed atomically with the conversation revision they record.
func activityConversationSubject(event *ActivityEvent) string {
	if event == nil || len(event.ConversationRefs) != 1 {
		return ""
	}
	return strings.TrimSpace(event.ConversationRefs[0])
}

func IsTerminalAgentRunStatus(status AgentRunStatus) bool {
	return status == AgentRunStatusCompleted || status == AgentRunStatusFailed || status == AgentRunStatusCanceled
}

func CloneActivityEvent(in *ActivityEvent) *ActivityEvent {
	if in == nil {
		return nil
	}
	var out ActivityEvent
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}

func ValidateObjectiveActivity(objective *Objective, event *ActivityEvent) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if objective.Scope != event.Scope || objective.ID != event.ObjectiveID || event.RunID != "" {
		return ErrInvalidScope
	}
	return nil
}

func CloneBudgetUsage(usage *BudgetUsage) *BudgetUsage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

func CanTransitionAgentRun(from, to AgentRunStatus) bool {
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

func IsWaitingRunStatus(status AgentRunStatus) bool {
	return status == AgentRunStatusSleeping || status == AgentRunStatusWaitingForDependency ||
		status == AgentRunStatusWaitingForAgent || status == AgentRunStatusWaitingForApproval ||
		status == AgentRunStatusWaitingForEvent
}

// terminalCreditError reads the kernel-classified cause of a failed Run, never
// provider text, so only a Workspace credit refusal yields a credit error.
func TerminalCreditError(status AgentRunStatus, failure *RunFailureExplanation) *RunCreditError {
	if status != AgentRunStatusFailed || failure == nil || failure.Code != RunCreditErrorWorkspaceCreditsExhausted {
		return nil
	}
	return &RunCreditError{Code: RunCreditErrorWorkspaceCreditsExhausted}
}
