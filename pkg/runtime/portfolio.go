package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidScope      = errors.New("scope kind and id are required")
	ErrInvalidOwner      = errors.New("objective owner type and id are required")
	ErrObjectiveNotFound = errors.New("objective not found")
	ErrRunNotFound       = errors.New("run not found")
	ErrRevisionConflict  = errors.New("objective revision conflict")
	ErrRunIdempotency    = errors.New("run idempotency key was already used with different input")
	ErrInvalidAgentRun   = errors.New("invalid agent run")
	ErrInvalidRunCommand = errors.New("invalid run command")
)

// Scope is the portable ownership boundary for every kernel resource. Embedding
// applications can map it to their own tenancy or namespace model; standalone OpenSeal normally uses
// {kind: "local", id: "default"}.
type Scope struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (s Scope) Validate() error {
	if strings.TrimSpace(s.Kind) == "" || strings.TrimSpace(s.ID) == "" {
		return ErrInvalidScope
	}
	return nil
}

type OwnerType string

const (
	OwnerTypeAgent OwnerType = "agent"
	OwnerTypeTeam  OwnerType = "team"
)

type ObjectiveOwner struct {
	Type OwnerType `json:"type"`
	ID   string    `json:"id"`
}

func (o ObjectiveOwner) Validate() error {
	if (o.Type != OwnerTypeAgent && o.Type != OwnerTypeTeam) || strings.TrimSpace(o.ID) == "" {
		return ErrInvalidOwner
	}
	return nil
}

type ObjectiveStatus string

const (
	ObjectiveStatusDraft     ObjectiveStatus = "draft"
	ObjectiveStatusActive    ObjectiveStatus = "active"
	ObjectiveStatusPaused    ObjectiveStatus = "paused"
	ObjectiveStatusSatisfied ObjectiveStatus = "satisfied"
	ObjectiveStatusFailed    ObjectiveStatus = "failed"
	ObjectiveStatusRetired   ObjectiveStatus = "retired"
)

type Objective struct {
	ID               string                 `json:"id"`
	Scope            Scope                  `json:"scope"`
	Owner            ObjectiveOwner         `json:"owner"`
	Title            string                 `json:"title"`
	Goal             string                 `json:"goal"`
	Status           ObjectiveStatus        `json:"status"`
	Priority         int                    `json:"priority"`
	Cadence          map[string]interface{} `json:"cadence,omitempty"`
	EventRules       map[string]interface{} `json:"eventRules,omitempty"`
	Budget           map[string]interface{} `json:"budget,omitempty"`
	Constraints      map[string]interface{} `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{} `json:"successCriteria,omitempty"`
	ProgressSummary  string                 `json:"progressSummary,omitempty"`
	NextEvaluationAt *time.Time             `json:"nextEvaluationAt,omitempty"`
	Revision         int64                  `json:"revision"`
	CreatedAt        time.Time              `json:"createdAt"`
	UpdatedAt        time.Time              `json:"updatedAt"`
}

func (o *Objective) Validate() error {
	if o == nil {
		return errors.New("objective is required")
	}
	if err := o.Scope.Validate(); err != nil {
		return err
	}
	if err := o.Owner.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(o.Title) == "" || strings.TrimSpace(o.Goal) == "" {
		return errors.New("objective title and goal are required")
	}
	if o.Priority < 0 {
		return errors.New("objective priority cannot be negative")
	}
	return nil
}

type AgentRunStatus string

const (
	AgentRunStatusQueued               AgentRunStatus = "queued"
	AgentRunStatusPlanning             AgentRunStatus = "planning"
	AgentRunStatusRunning              AgentRunStatus = "running"
	AgentRunStatusPaused               AgentRunStatus = "paused"
	AgentRunStatusSleeping             AgentRunStatus = "sleeping"
	AgentRunStatusWaitingForDependency AgentRunStatus = "waiting_for_dependency"
	AgentRunStatusWaitingForAgent      AgentRunStatus = "waiting_for_agent"
	AgentRunStatusWaitingForApproval   AgentRunStatus = "waiting_for_approval"
	AgentRunStatusWaitingForEvent      AgentRunStatus = "waiting_for_event"
	AgentRunStatusCompleted            AgentRunStatus = "completed"
	AgentRunStatusFailed               AgentRunStatus = "failed"
	AgentRunStatusCanceled             AgentRunStatus = "canceled"
)

type RunSource string

const (
	RunSourceManual    RunSource = "manual"
	RunSourceChat      RunSource = "chat"
	RunSourceSchedule  RunSource = "schedule"
	RunSourceEvent     RunSource = "event"
	RunSourceWebhook   RunSource = "webhook"
	RunSourceRequest   RunSource = "agent_request"
	RunSourceHandoff   RunSource = "handoff"
	RunSourceObjective RunSource = "objective"
)

// RunKind identifies the execution contract a durable Run requires. Ownership
// remains independent: both Agents and Teams can own agent work, while system
// workers can claim only the explicit kernel work they implement.
type RunKind string

const (
	RunKindAgentWork    RunKind = "agent_work"
	RunKindConversation RunKind = "conversation"
)

type WakeCondition struct {
	Type      string                 `json:"type"`
	WakeAt    *time.Time             `json:"wakeAt,omitempty"`
	Reference string                 `json:"reference,omitempty"`
	Predicate map[string]interface{} `json:"predicate,omitempty"`
}

// AgentRunIntervention is durable operator steering. It records an explicit
// instruction without exposing private model reasoning or mutating the
// agent's definition.
type AgentRunIntervention struct {
	ID          string        `json:"id"`
	Actor       ActivityActor `json:"actor"`
	Instruction string        `json:"instruction"`
	CreatedAt   time.Time     `json:"createdAt"`
}

// AgentRun is the canonical durable workstream. Workflow execution records are
// subordinate execution details and must not be used as agent-run identity.
type AgentRun struct {
	ID                   string                 `json:"id"`
	Kind                 RunKind                `json:"kind"`
	Scope                Scope                  `json:"scope"`
	ObjectiveID          string                 `json:"objectiveId,omitempty"`
	ParentRunID          string                 `json:"parentRunId,omitempty"`
	RootRunID            string                 `json:"rootRunId"`
	Owner                ObjectiveOwner         `json:"owner"`
	AssignedAgentID      string                 `json:"assignedAgentId,omitempty"`
	Goal                 string                 `json:"goal"`
	Source               RunSource              `json:"source"`
	Status               AgentRunStatus         `json:"status"`
	Priority             int                    `json:"priority"`
	Deadline             *time.Time             `json:"deadline,omitempty"`
	AvailableAt          time.Time              `json:"availableAt"`
	QueueEnteredAt       time.Time              `json:"queueEnteredAt"`
	LeaseOwner           string                 `json:"leaseOwner,omitempty"`
	LeaseExpiresAt       *time.Time             `json:"leaseExpiresAt,omitempty"`
	LastClaimedAt        *time.Time             `json:"lastClaimedAt,omitempty"`
	Attempt              int                    `json:"attempt"`
	LastWakeSignalID     string                 `json:"lastWakeSignalId,omitempty"`
	Context              map[string]interface{} `json:"context,omitempty"`
	Plan                 map[string]interface{} `json:"plan,omitempty"`
	Checkpoint           map[string]interface{} `json:"checkpoint,omitempty"`
	WakeCondition        *WakeCondition         `json:"wakeCondition,omitempty"`
	PausedFrom           AgentRunStatus         `json:"pausedFrom,omitempty"`
	PausedWakeCondition  *WakeCondition         `json:"pausedWakeCondition,omitempty"`
	PendingInterventions []AgentRunIntervention `json:"pendingInterventions,omitempty"`
	Budget               map[string]interface{} `json:"budget,omitempty"`
	Policy               map[string]interface{} `json:"policy,omitempty"`
	Output               map[string]interface{} `json:"output,omitempty"`
	Error                string                 `json:"error,omitempty"`
	WorkflowExecution    *int                   `json:"workflowExecutionId,omitempty"`
	LastAppliedTurn      int64                  `json:"lastAppliedTurn"`
	Revision             int64                  `json:"revision"`
	CreatedAt            time.Time              `json:"createdAt"`
	UpdatedAt            time.Time              `json:"updatedAt"`
	StartedAt            *time.Time             `json:"startedAt,omitempty"`
	CompletedAt          *time.Time             `json:"completedAt,omitempty"`
	IdempotencyKeyHash   string                 `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint  string                 `json:"creationFingerprint,omitempty"`
}

func (r *AgentRun) Validate() error {
	if r == nil {
		return errors.New("run is required")
	}
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if err := r.Owner.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Goal) == "" {
		return errors.New("run goal is required")
	}
	if r.Kind != "" && !validRunKind(r.Kind) {
		return fmt.Errorf("unsupported run kind %q", r.Kind)
	}
	if r.Priority < 0 {
		return errors.New("run priority cannot be negative")
	}
	return nil
}

type ObjectiveFilter struct {
	Scope          Scope
	Owner          *ObjectiveOwner
	Statuses       []ObjectiveStatus
	IncludeRetired bool
	Limit          int
	Offset         int
}

type AgentRunFilter struct {
	Scope           Scope
	Kind            RunKind
	Owner           *ObjectiveOwner
	ObjectiveID     string
	ParentRunID     string
	RootRunID       string
	AssignedAgentID string
	Statuses        []AgentRunStatus
	Limit           int
	Offset          int
}

// PortfolioStore persists the multi-objective agent kernel independently from
// any one enterprise database implementation.
type PortfolioStore interface {
	CreateObjective(ctx context.Context, objective *Objective) error
	GetObjective(ctx context.Context, scope Scope, objectiveID string) (*Objective, error)
	ListObjectives(ctx context.Context, filter ObjectiveFilter) ([]*Objective, error)
	UpdateObjective(ctx context.Context, objective *Objective, expectedRevision int64) error
	CreateAgentRun(ctx context.Context, run *AgentRun) error
	GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error)
	ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error)
}

// KernelStore is the complete persistence contract required by the OpenSeal
// Engine. Embedding applications can supply an implementation; standalone
// OpenSeal ships memory and SQLite implementations.
type KernelStore interface {
	ExecutionStore
	PortfolioStore
	RunActivityStore
	AgentTurnStore
	AgentRunScheduleStore
	ActionStore
}

type CreateObjectiveRequest struct {
	Scope            Scope
	Owner            ObjectiveOwner
	Title            string
	Goal             string
	Status           ObjectiveStatus
	Priority         int
	Cadence          map[string]interface{}
	EventRules       map[string]interface{}
	Budget           map[string]interface{}
	Constraints      map[string]interface{}
	SuccessCriteria  map[string]interface{}
	NextEvaluationAt *time.Time
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64
	Title            *string
	Goal             *string
	Status           *ObjectiveStatus
	Priority         *int
	Cadence          map[string]interface{}
	EventRules       map[string]interface{}
	Budget           map[string]interface{}
	Constraints      map[string]interface{}
	SuccessCriteria  map[string]interface{}
	ProgressSummary  *string
	NextEvaluationAt *time.Time
}

type CreateAgentRunRequest struct {
	Scope           Scope
	Kind            RunKind
	ObjectiveID     string
	ParentRunID     string
	Owner           ObjectiveOwner
	AssignedAgentID string
	Goal            string
	Source          RunSource
	Priority        int
	Deadline        *time.Time
	AvailableAt     *time.Time
	Context         map[string]interface{}
	Plan            map[string]interface{}
	Checkpoint      map[string]interface{}
	WakeCondition   *WakeCondition
	Budget          map[string]interface{}
	Policy          map[string]interface{}
	IdempotencyKey  string
	Actor           ActivityActor
	Visibility      ActivityVisibility
}

type PortfolioService struct {
	store PortfolioStore
	now   func() time.Time
}

func NewPortfolioService(store PortfolioStore) *PortfolioService {
	return &PortfolioService{store: store, now: time.Now}
}

func (s *PortfolioService) CreateObjective(ctx context.Context, req CreateObjectiveRequest) (*Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	now := s.now()
	status := req.Status
	if status == "" {
		status = ObjectiveStatusDraft
	}
	objective := &Objective{
		ID: uuid.NewString(), Scope: req.Scope, Owner: req.Owner, Title: req.Title,
		Goal: req.Goal, Status: status, Priority: req.Priority, Cadence: req.Cadence,
		EventRules: req.EventRules, Budget: req.Budget, Constraints: req.Constraints,
		SuccessCriteria: req.SuccessCriteria, NextEvaluationAt: req.NextEvaluationAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := objective.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateObjective(ctx, objective); err != nil {
		return nil, err
	}
	return objective, nil
}

func (s *PortfolioService) UpdateObjective(ctx context.Context, scope Scope, objectiveID string, req UpdateObjectiveRequest) (*Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	current, err := s.store.GetObjective(ctx, scope, objectiveID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrObjectiveNotFound
	}
	if req.ExpectedRevision != current.Revision {
		return nil, ErrRevisionConflict
	}
	applyObjectiveUpdate(current, req)
	current.UpdatedAt = s.now()
	current.Revision++
	if err := current.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.UpdateObjective(ctx, current, req.ExpectedRevision); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *PortfolioService) GetObjective(ctx context.Context, scope Scope, objectiveID string) (*Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	objective, err := s.store.GetObjective(ctx, scope, objectiveID)
	if err != nil {
		return nil, err
	}
	if objective == nil {
		return nil, ErrObjectiveNotFound
	}
	return objective, nil
}

func (s *PortfolioService) ListObjectives(ctx context.Context, filter ObjectiveFilter) ([]*Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListObjectives(ctx, filter)
}

func (s *PortfolioService) CreateAgentRun(ctx context.Context, req CreateAgentRunRequest) (*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	run, err := buildAgentRun(ctx, s.store, req, uuid.NewString(), s.now())
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateAgentRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

func buildAgentRun(ctx context.Context, store PortfolioStore, req CreateAgentRunRequest, runID string, now time.Time) (*AgentRun, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	if err := req.Owner.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	for _, state := range []map[string]interface{}{req.Context, req.Plan, req.Checkpoint, req.Policy} {
		if err := ValidateCredentialFreeContext(state); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
		}
	}
	if req.ObjectiveID != "" {
		objective, err := store.GetObjective(ctx, req.Scope, req.ObjectiveID)
		if err != nil {
			return nil, err
		}
		if objective == nil {
			return nil, ErrObjectiveNotFound
		}
	}
	availableAt := now
	if req.AvailableAt != nil {
		availableAt = *req.AvailableAt
	}
	rootID := runID
	if req.ParentRunID != "" {
		parent, err := store.GetAgentRun(ctx, req.Scope, req.ParentRunID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, ErrRunNotFound
		}
		rootID = parent.RootRunID
	}
	source := req.Source
	if source == "" {
		source = RunSourceManual
	}
	switch source {
	case RunSourceManual, RunSourceChat, RunSourceSchedule, RunSourceEvent, RunSourceWebhook, RunSourceRequest, RunSourceHandoff, RunSourceObjective:
	default:
		return nil, fmt.Errorf("%w: unsupported run source %q", ErrInvalidAgentRun, source)
	}
	kind := normalizeRunKind(req.Kind)
	if !validRunKind(kind) {
		return nil, fmt.Errorf("%w: unsupported run kind %q", ErrInvalidAgentRun, req.Kind)
	}
	run := &AgentRun{
		ID: runID, Kind: kind, Scope: req.Scope, ObjectiveID: req.ObjectiveID,
		ParentRunID: req.ParentRunID, RootRunID: rootID, Owner: req.Owner,
		AssignedAgentID: req.AssignedAgentID, Goal: req.Goal, Source: source,
		Status: AgentRunStatusQueued, Priority: req.Priority, Deadline: req.Deadline,
		AvailableAt: availableAt, QueueEnteredAt: now, Context: req.Context,
		Plan: req.Plan, Checkpoint: req.Checkpoint, WakeCondition: req.WakeCondition,
		Budget: req.Budget, Policy: req.Policy, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := run.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	return run, nil
}

func normalizeRunKind(kind RunKind) RunKind {
	if kind == "" {
		return RunKindAgentWork
	}
	return kind
}

func validRunKind(kind RunKind) bool {
	switch kind {
	case RunKindAgentWork, RunKindConversation:
		return true
	default:
		return false
	}
}

func (s *PortfolioService) GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	run, err := s.store.GetAgentRun(ctx, scope, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	return run, nil
}

func (s *PortfolioService) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.Kind != "" && !validRunKind(filter.Kind) {
		return nil, fmt.Errorf("%w: unsupported run kind %q", ErrInvalidAgentRun, filter.Kind)
	}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
	}
	return s.store.ListAgentRuns(ctx, filter)
}

func applyObjectiveUpdate(objective *Objective, req UpdateObjectiveRequest) {
	if req.Title != nil {
		objective.Title = *req.Title
	}
	if req.Goal != nil {
		objective.Goal = *req.Goal
	}
	if req.Status != nil {
		objective.Status = *req.Status
	}
	if req.Priority != nil {
		objective.Priority = *req.Priority
	}
	if req.Cadence != nil {
		objective.Cadence = req.Cadence
	}
	if req.EventRules != nil {
		objective.EventRules = req.EventRules
	}
	if req.Budget != nil {
		objective.Budget = req.Budget
	}
	if req.Constraints != nil {
		objective.Constraints = req.Constraints
	}
	if req.SuccessCriteria != nil {
		objective.SuccessCriteria = req.SuccessCriteria
	}
	if req.ProgressSummary != nil {
		objective.ProgressSummary = *req.ProgressSummary
	}
	if req.NextEvaluationAt != nil {
		objective.NextEvaluationAt = req.NextEvaluationAt
	}
}

func (s Scope) key() string { return fmt.Sprintf("%s:%s", s.Kind, s.ID) }
