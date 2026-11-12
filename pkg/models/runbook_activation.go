package models

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

var (
	ErrRunbookActivationNotFound    = errors.New("Runbook activation not found")
	ErrRunbookActivationRevision    = errors.New("Runbook activation revision conflict")
	ErrRunbookActivationIdempotency = errors.New("Runbook activation idempotency key was already used with different input")
)

type RunbookDefinitionCatalog interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	ListDefinitionVersions(context.Context, string) ([]*kernelagent.AgentDefinition, error)
}

type RunbookActivationReader interface {
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
}

type RunbookActivationStatus string

const (
	RunbookActivationActive  RunbookActivationStatus = "active"
	RunbookActivationPaused  RunbookActivationStatus = "paused"
	RunbookActivationRetired RunbookActivationStatus = "retired"
)

// RunbookActivation is the owner-scoped executable Runbook shown beneath one
// Objective. The immutable definition describes the method; this activation
// binds its exact trigger, target, inputs, policy, and operational limits.
// Scheduling state therefore belongs to the Runbook and never to the outcome.
type RunbookActivation struct {
	LifecycleActionID    string                  `json:"lifecycleActionId,omitempty"`
	Task                 *ScheduledAgentTask     `json:"task,omitempty"`
	ID                   string                  `json:"id"`
	Scope                Scope                   `json:"scope"`
	Owner                ObjectiveOwner          `json:"owner"`
	ObjectiveID          string                  `json:"objectiveId"`
	AssignedAgentID      string                  `json:"assignedAgentId"`
	DefinitionID         string                  `json:"definitionId"`
	DefinitionVersion    string                  `json:"definitionVersion"`
	TriggerID            string                  `json:"triggerId"`
	Trigger              runbook.Trigger         `json:"trigger"`
	Input                map[string]interface{}  `json:"input,omitempty"`
	Policy               map[string]interface{}  `json:"policy,omitempty"`
	Budget               *BudgetPolicy           `json:"budget,omitempty"`
	MaximumConcurrent    int                     `json:"maximumConcurrent,omitempty"`
	Status               RunbookActivationStatus `json:"status"`
	NextOccurrenceBase   *time.Time              `json:"nextOccurrenceBase,omitempty"`
	NextRunAt            *time.Time              `json:"nextRunAt,omitempty"`
	OccurrencesProcessed int64                   `json:"occurrencesProcessed,omitempty"`
	// PausedReason says why the system paused the routine. It is set only
	// with Status paused and cleared on any other status change.
	PausedReason RoutinePausedReason `json:"pausedReason,omitempty"`
	// Exhausted is set when the event dispatcher retires an event routine
	// because it started its maximum of real batches. It is set only with
	// Status retired; a routine the person cancelled never has it.
	Exhausted           bool      `json:"exhausted,omitempty"`
	Revision            int64     `json:"revision"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
	IdempotencyKeyHash  string    `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string    `json:"creationFingerprint,omitempty"`
}

// Callable reports whether an activation can be started on demand. A schedule
// that reached its reviewed occurrence ceiling is no longer scheduler-active,
// but its immutable reviewed operation remains safe to invoke off-cycle. An
// event Task the dispatcher retired as exhausted is callable the same way.
// Paused activations and routines the person retired remain unavailable.
func (a *RunbookActivation) Callable() bool {
	if a == nil {
		return false
	}
	if a.Status == RunbookActivationActive {
		return true
	}
	if a.Status == RunbookActivationRetired && a.Exhausted && a.Task != nil && a.Task.Event != nil && a.Trigger.Kind == runbook.TriggerEvent {
		return true
	}
	return a.Status == RunbookActivationRetired && a.Trigger.Kind == runbook.TriggerSchedule &&
		a.Trigger.Schedule != nil && a.Trigger.Schedule.MaximumOccurrences > 0 &&
		a.OccurrencesProcessed >= a.Trigger.Schedule.MaximumOccurrences
}

func (a *RunbookActivation) Validate() error {
	if a == nil {
		return errors.New("Runbook activation is required")
	}
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if err := a.Owner.Validate(); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"id": a.ID, "objectiveId": a.ObjectiveID, "assignedAgentId": a.AssignedAgentID,
		"triggerId": a.TriggerID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Runbook activation %s is required", field)
		}
	}
	if a.Task != nil {
		if err := a.Task.Validate(); err != nil {
			return err
		}
		if a.Trigger.Entrypoint != "" || a.DefinitionID != "" || a.DefinitionVersion != "" {
			return errors.New("scheduled Agent task cannot specify a Runbook definition or entrypoint")
		}
		switch a.Trigger.Kind {
		case runbook.TriggerSchedule:
			if a.Task.Event != nil {
				return errors.New("scheduled Agent task cannot have an event trigger")
			}
		case runbook.TriggerEvent:
			if a.Task.Event == nil {
				return errors.New("event Agent task requires its event trigger")
			}
			if !onlyEventTriggerKind(a.Trigger) {
				return errors.New("event Agent task trigger can only name its kind")
			}
			if strings.TrimSpace(a.Task.CreatedBy) == "" {
				return errors.New("event Agent task requires the person who created it")
			}
		default:
			return errors.New("Runbook activation trigger kind is invalid")
		}
	} else {
		if strings.TrimSpace(a.DefinitionID) == "" || strings.TrimSpace(a.DefinitionVersion) == "" || strings.TrimSpace(a.Trigger.Entrypoint) == "" {
			return errors.New("Runbook activation trigger entrypoint is required")
		}
		if a.Trigger.Kind != runbook.TriggerSchedule {
			return errors.New("Runbook definition activations can only run on a schedule")
		}
	}
	if a.Trigger.Kind == runbook.TriggerSchedule {
		if a.Trigger.Schedule == nil {
			return errors.New("Runbook activation schedule trigger is required")
		}
		if err := a.Trigger.Schedule.Validate(); err != nil {
			return fmt.Errorf("Runbook activation trigger: %w", err)
		}
	}
	if err := ValidateCredentialFreeContext(a.Input); err != nil {
		return fmt.Errorf("Runbook activation input: %w", err)
	}
	if err := ValidateCredentialFreeContext(a.Policy); err != nil {
		return fmt.Errorf("Runbook activation policy: %w", err)
	}
	if a.Budget != nil {
		if err := a.Budget.Validate(); err != nil {
			return fmt.Errorf("Runbook activation budget: %w", err)
		}
	}
	if a.MaximumConcurrent < 0 {
		return errors.New("Runbook activation maximumConcurrent cannot be negative")
	}
	if a.OccurrencesProcessed < 0 || a.Trigger.Schedule != nil && a.Trigger.Schedule.MaximumOccurrences > 0 && a.OccurrencesProcessed > a.Trigger.Schedule.MaximumOccurrences {
		return errors.New("Runbook activation occurrence progress is invalid")
	}
	switch a.Status {
	case RunbookActivationActive, RunbookActivationPaused, RunbookActivationRetired:
	default:
		return errors.New("Runbook activation status is invalid")
	}
	if a.PausedReason != "" && (a.Status != RunbookActivationPaused || !IsValidRoutinePausedReason(a.PausedReason)) {
		return errors.New("Runbook activation paused reason is invalid")
	}
	if a.Exhausted && (a.Status != RunbookActivationRetired || a.Trigger.Kind != runbook.TriggerEvent) {
		return errors.New("only a retired event Runbook activation can be exhausted")
	}
	if a.Revision < 1 || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) {
		return errors.New("Runbook activation requires valid revision and timestamps")
	}
	if a.Trigger.Kind == runbook.TriggerEvent && (a.NextOccurrenceBase != nil || a.NextRunAt != nil) {
		return errors.New("event Runbook activation cannot have a schedule cursor")
	}
	if a.Trigger.Kind == runbook.TriggerEvent && a.OccurrencesProcessed != 0 {
		return errors.New("event Runbook activation cannot have schedule occurrence progress")
	}
	if (a.NextOccurrenceBase == nil) != (a.NextRunAt == nil) {
		return errors.New("Runbook activation schedule cursor requires both base occurrence and due time")
	}
	if a.NextOccurrenceBase != nil {
		start, end, err := a.Trigger.Schedule.Window(*a.NextOccurrenceBase)
		if err != nil || a.NextRunAt.Before(start) || a.NextRunAt.After(end) {
			return errors.New("Runbook activation nextRunAt must be inside its cron jitter window")
		}
	}
	return nil
}

// onlyEventTriggerKind reports whether an event Task's trigger names nothing
// but its kind; everything else about the event lives in Task.Event.
func onlyEventTriggerKind(t runbook.Trigger) bool {
	return t.Kind == runbook.TriggerEvent && t.Schedule == nil && t.Entrypoint == "" && t.ObjectiveID == "" &&
		len(t.Input) == 0 && t.Budget == nil && t.Reporting == nil && t.MaximumConcurrent == 0
}

func IsValidRoutinePausedReason(reason RoutinePausedReason) bool {
	switch reason {
	case RoutinePausedNeedsSignIn, RoutinePausedSourceRemoved, RoutinePausedUnverified:
		return true
	}
	return false
}

type RunbookActivationFilter struct {
	Scope        Scope
	Owner        *ObjectiveOwner
	ObjectiveID  string
	Statuses     []RunbookActivationStatus
	TriggerKinds []runbook.TriggerKind
	Limit        int
	Offset       int
}

type RunbookActivationStore interface {
	CreateRunbookActivation(context.Context, *RunbookActivation) error
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
	ListRunbookActivations(context.Context, RunbookActivationFilter) ([]*RunbookActivation, error)
	UpdateRunbookActivation(context.Context, *RunbookActivation, int64) error
	ListRunbookActivationScopes(context.Context) ([]Scope, error)
}

func MatchesRunbookActivationFilter(value *RunbookActivation, filter RunbookActivationFilter) bool {
	if value == nil || value.Scope != filter.Scope {
		return false
	}
	if filter.Owner != nil && value.Owner != *filter.Owner {
		return false
	}
	if strings.TrimSpace(filter.ObjectiveID) != "" && value.ObjectiveID != strings.TrimSpace(filter.ObjectiveID) {
		return false
	}
	if len(filter.Statuses) > 0 {
		matched := false
		for _, status := range filter.Statuses {
			matched = matched || value.Status == status
		}
		if !matched {
			return false
		}
	}
	if len(filter.TriggerKinds) > 0 {
		matched := false
		for _, kind := range filter.TriggerKinds {
			matched = matched || value.Trigger.Kind == kind
		}
		if !matched {
			return false
		}
	}
	return true
}

func SortAndLimitRunbookActivations(values []*RunbookActivation, limit, offset int) []*RunbookActivation {
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*RunbookActivation{}
	}
	values = values[offset:]
	if len(values) > limit {
		values = values[:limit]
	}
	return values
}

func CloneRunbookActivation(value *RunbookActivation) *RunbookActivation {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Task = CloneScheduledAgentTask(value.Task)
	clone.Input = maps.Clone(value.Input)
	clone.Policy = maps.Clone(value.Policy)
	clone.Budget = CloneBudgetPolicy(value.Budget)
	if value.NextOccurrenceBase != nil {
		base := *value.NextOccurrenceBase
		clone.NextOccurrenceBase = &base
	}
	if value.NextRunAt != nil {
		due := *value.NextRunAt
		clone.NextRunAt = &due
	}
	return &clone
}

// RunbookExecutionAuditStore is the portable read boundary required to
// reconstruct one Runbook occurrence. Implementations already persist these
// canonical facts; the audit service creates no parallel execution state.
type RunbookExecutionAuditStore interface {
	GetAgentRun(context.Context, Scope, string) (*AgentRun, error)
	ListAgentRuns(context.Context, AgentRunFilter) ([]*AgentRun, error)
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
	ListAgentTurns(context.Context, AgentTurnFilter) ([]*AgentTurn, error)
	ListActionCalls(context.Context, ActionFilter) ([]*ActionCall, error)
	ListApprovals(context.Context, ApprovalFilter) ([]*ApprovalCheckpoint, error)
	ListArtifacts(context.Context, ArtifactFilter) ([]*Artifact, error)
	ListAgentRequests(context.Context, AgentRequestFilter) ([]*AgentRequest, error)
}

func BudgetPolicyFromRunbook(budget *runbook.BudgetAllocation) *BudgetPolicy {
	if budget == nil {
		return nil
	}
	return &BudgetPolicy{
		MaxAttempts: budget.MaxAttempts,
		MaxTurns:    budget.MaxTurns, MaxInputTokens: budget.MaxInputTokens,
		MaxOutputTokens: budget.MaxOutputTokens, MaxTotalTokens: budget.MaxTotalTokens,
		MaxCostMicros: budget.MaxCostMicros, MaxDurationMS: budget.MaxDurationMS,
		MaxActions: budget.MaxActions, WarningPermille: budget.WarningPermille,
	}
}
