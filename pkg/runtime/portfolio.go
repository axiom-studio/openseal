package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidScope               = errors.New("scope kind and id are required")
	ErrInvalidOwner               = errors.New("objective owner type and id are required")
	ErrObjectiveNotFound          = errors.New("objective not found")
	ErrObjectiveIdempotency       = errors.New("objective idempotency key was already used with different input")
	ErrInvalidObjectiveTransition = errors.New("invalid objective transition")
	ErrRunNotFound                = errors.New("run not found")
	ErrRevisionConflict           = errors.New("objective revision conflict")
	ErrRunIdempotency             = errors.New("run idempotency key was already used with different input")
	ErrInvalidAgentRun            = errors.New("invalid agent run")
	ErrInvalidRunCommand          = errors.New("invalid run command")
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
	ID                  string                    `json:"id"`
	Scope               Scope                     `json:"scope"`
	Owner               ObjectiveOwner            `json:"owner"`
	Title               string                    `json:"title"`
	Goal                string                    `json:"goal"`
	Status              ObjectiveStatus           `json:"status"`
	Priority            int                       `json:"priority"`
	ExecutionPolicy     *ObjectiveExecutionPolicy `json:"executionPolicy,omitempty"`
	Budget              *BudgetPolicy             `json:"budget,omitempty"`
	BudgetAllocations   map[string]BudgetPolicy   `json:"budgetAllocations,omitempty"`
	Constraints         map[string]interface{}    `json:"constraints,omitempty"`
	SuccessCriteria     map[string]interface{}    `json:"successCriteria,omitempty"`
	ProgressSummary     string                    `json:"progressSummary,omitempty"`
	Revision            int64                     `json:"revision"`
	CreatedAt           time.Time                 `json:"createdAt"`
	UpdatedAt           time.Time                 `json:"updatedAt"`
	IdempotencyKeyHash  string                    `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string                    `json:"creationFingerprint,omitempty"`
}

// ObjectiveExecutionPolicy is the durable admission policy for work owned by
// an Objective. A nil policy inherits the embedding runtime's portfolio
// defaults; an explicit zero maximum means the Objective itself is unbounded
// while still respecting stricter owner, Agent, and runtime ceilings.
type ObjectiveExecutionPolicy struct {
	MaximumConcurrentRuns int            `json:"maximumConcurrentRuns"`
	ResourceCapacities    map[string]int `json:"resourceCapacities,omitempty"`
}

func (p *ObjectiveExecutionPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if p.MaximumConcurrentRuns < 0 {
		return errors.New("objective maximum concurrent runs cannot be negative")
	}
	if err := validateResourceQuantities(p.ResourceCapacities, true); err != nil {
		return fmt.Errorf("objective resource capacities: %w", err)
	}
	return nil
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
	if err := o.ExecutionPolicy.Validate(); err != nil {
		return err
	}
	if !validObjectiveStatus(o.Status) {
		return errors.New("objective status is invalid")
	}
	if o.Budget != nil {
		if err := o.Budget.Validate(); err != nil {
			return err
		}
		if err := validatePolicyAllocations(*o.Budget, o.BudgetAllocations); err != nil {
			return err
		}
	} else if len(o.BudgetAllocations) > 0 {
		return errors.New("objective budget allocations require an objective budget")
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
	RunSourceManual           RunSource = "manual"
	RunSourceChat             RunSource = "chat"
	RunSourceSchedule         RunSource = "schedule"
	RunSourceEvent            RunSource = "event"
	RunSourceWebhook          RunSource = "webhook"
	RunSourceRequest          RunSource = "agent_request"
	RunSourceRequestDecision  RunSource = "agent_request_decision"
	RunSourceCompletionReview RunSource = "agent_request_completion_review"
	RunSourceHandoff          RunSource = "handoff"
	RunSourceEscalation       RunSource = "escalation"
	RunSourceObjective        RunSource = "objective"
	RunSourceFork             RunSource = "fork"
)

// RunKind identifies the execution contract a durable Run requires. Ownership
// remains independent: both Agents and Teams can own agent work, while system
// workers can claim only the explicit kernel work they implement.
type RunKind string

const (
	RunKindAgentWork          RunKind = "agent_work"
	RunKindConversation       RunKind = "conversation"
	RunKindWorkforceAuthoring RunKind = "workforce_authoring"
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

type HumanInterventionStatus string

const (
	HumanInterventionStatusPending  HumanInterventionStatus = "pending"
	HumanInterventionStatusResolved HumanInterventionStatus = "resolved"
	HumanInterventionStatusCanceled HumanInterventionStatus = "canceled"
)

// HumanInterventionRequest is a typed, durable request emitted when a
// capability cannot safely continue without a person. It deliberately stores
// only bounded classification and provenance; capability output, page content,
// credentials, and hidden model state are not copied into the request.
type HumanInterventionRequest struct {
	ID           string                  `json:"id"`
	Kind         string                  `json:"kind"`
	Status       HumanInterventionStatus `json:"status"`
	ActionCallID string                  `json:"actionCallId"`
	Summary      string                  `json:"summary"`
	Challenge    []string                `json:"challenge,omitempty"`
	Resolution   string                  `json:"resolution,omitempty"`
	ResolvedBy   *ActivityActor          `json:"resolvedBy,omitempty"`
	CreatedAt    time.Time               `json:"createdAt"`
	ResolvedAt   *time.Time              `json:"resolvedAt,omitempty"`
}

// AgentRun is the canonical durable workstream. Workflow execution records are
// subordinate execution details and must not be used as agent-run identity.
type AgentRun struct {
	ID                   string                       `json:"id"`
	Kind                 RunKind                      `json:"kind"`
	Scope                Scope                        `json:"scope"`
	ObjectiveID          string                       `json:"objectiveId,omitempty"`
	ParentRunID          string                       `json:"parentRunId,omitempty"`
	RootRunID            string                       `json:"rootRunId"`
	Owner                ObjectiveOwner               `json:"owner"`
	AssignedAgentID      string                       `json:"assignedAgentId,omitempty"`
	Entrypoint           string                       `json:"entrypoint,omitempty"`
	ConcurrencyKey       string                       `json:"concurrencyKey,omitempty"`
	ResourceRequirements map[string]int               `json:"resourceRequirements,omitempty"`
	Goal                 string                       `json:"goal"`
	Source               RunSource                    `json:"source"`
	Status               AgentRunStatus               `json:"status"`
	Priority             int                          `json:"priority"`
	Deadline             *time.Time                   `json:"deadline,omitempty"`
	AvailableAt          time.Time                    `json:"availableAt"`
	QueueEnteredAt       time.Time                    `json:"queueEnteredAt"`
	LeaseOwner           string                       `json:"leaseOwner,omitempty"`
	LeaseExpiresAt       *time.Time                   `json:"leaseExpiresAt,omitempty"`
	LastClaimedAt        *time.Time                   `json:"lastClaimedAt,omitempty"`
	Attempt              int                          `json:"attempt"`
	LastWakeSignalID     string                       `json:"lastWakeSignalId,omitempty"`
	Context              map[string]interface{}       `json:"context,omitempty"`
	Plan                 map[string]interface{}       `json:"plan,omitempty"`
	Checkpoint           map[string]interface{}       `json:"checkpoint,omitempty"`
	WakeCondition        *WakeCondition               `json:"wakeCondition,omitempty"`
	PausedFrom           AgentRunStatus               `json:"pausedFrom,omitempty"`
	PausedWakeCondition  *WakeCondition               `json:"pausedWakeCondition,omitempty"`
	PendingInterventions []AgentRunIntervention       `json:"pendingInterventions,omitempty"`
	HumanInterventions   []HumanInterventionRequest   `json:"humanInterventions,omitempty"`
	Budget               *BudgetPolicy                `json:"budget,omitempty"`
	BudgetUsage          BudgetUsage                  `json:"budgetUsage,omitempty"`
	BudgetState          BudgetState                  `json:"budgetState,omitempty"`
	BudgetAdmission      *BudgetAdmission             `json:"budgetAdmission,omitempty"`
	BudgetReservations   map[string]BudgetReservation `json:"budgetReservations,omitempty"`
	BudgetAllocations    map[string]BudgetPolicy      `json:"budgetAllocations,omitempty"`
	Policy               map[string]interface{}       `json:"policy,omitempty"`
	Output               map[string]interface{}       `json:"output,omitempty"`
	Error                string                       `json:"error,omitempty"`
	WorkflowExecution    *int                         `json:"workflowExecutionId,omitempty"`
	LastAppliedTurn      int64                        `json:"lastAppliedTurn"`
	Revision             int64                        `json:"revision"`
	CreatedAt            time.Time                    `json:"createdAt"`
	UpdatedAt            time.Time                    `json:"updatedAt"`
	StartedAt            *time.Time                   `json:"startedAt,omitempty"`
	CompletedAt          *time.Time                   `json:"completedAt,omitempty"`
	IdempotencyKeyHash   string                       `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint  string                       `json:"creationFingerprint,omitempty"`
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
	if r.Budget != nil {
		if err := r.Budget.Validate(); err != nil {
			return err
		}
		if err := r.BudgetUsage.Validate(); err != nil {
			return err
		}
		for id, allocation := range r.BudgetAllocations {
			if strings.TrimSpace(id) == "" {
				return errors.New("run budget allocation id is required")
			}
			if err := allocation.Validate(); err != nil {
				return fmt.Errorf("run budget allocation %s: %w", id, err)
			}
		}
		effective, err := EffectiveBudgetUsage(r.BudgetUsage, r.BudgetReservations)
		if err != nil {
			return err
		}
		state, _, err := EvaluateBudget(*r.Budget, effective)
		if err != nil {
			return err
		}
		if r.BudgetAdmission != nil {
			if err := r.BudgetAdmission.Validate(); err != nil {
				return err
			}
			state = BudgetStateExhausted
		}
		if r.BudgetState != "" && r.BudgetState != state {
			return errors.New("run budget state does not match its policy and usage")
		}
	}
	if len(r.ConcurrencyKey) > 256 || strings.ContainsAny(r.ConcurrencyKey, "\r\n") {
		return errors.New("run concurrency key cannot exceed 256 characters or contain line breaks")
	}
	if err := validateResourceQuantities(r.ResourceRequirements, false); err != nil {
		return fmt.Errorf("run resource requirements: %w", err)
	}
	seenHumanInterventions := make(map[string]struct{}, len(r.HumanInterventions))
	for _, request := range r.HumanInterventions {
		if strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.Kind) == "" || strings.TrimSpace(request.ActionCallID) == "" || strings.TrimSpace(request.Summary) == "" {
			return errors.New("human intervention identity, kind, action call, and summary are required")
		}
		if _, exists := seenHumanInterventions[request.ID]; exists {
			return errors.New("human intervention ids must be unique")
		}
		seenHumanInterventions[request.ID] = struct{}{}
		if request.Status != HumanInterventionStatusPending && request.Status != HumanInterventionStatusResolved && request.Status != HumanInterventionStatusCanceled {
			return errors.New("human intervention status is invalid")
		}
		if request.Status == HumanInterventionStatusPending && (request.ResolvedAt != nil || request.ResolvedBy != nil || strings.TrimSpace(request.Resolution) != "") {
			return errors.New("pending human intervention cannot contain a resolution")
		}
		if request.Status != HumanInterventionStatusPending && request.ResolvedAt == nil {
			return errors.New("terminal human intervention requires a resolution timestamp")
		}
		if err := uniqueIDs(request.Challenge, "human intervention challenge"); err != nil {
			return err
		}
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
	ConversationID  string // Exact canonical conversation ID stored in run context.
	Query           string // Literal case-insensitive substring of the goal or status.
	Scope           Scope
	Kind            RunKind
	Owner           *ObjectiveOwner
	ObjectiveID     string
	ParentRunID     string
	RootRunID       string
	AssignedAgentID string
	Statuses        []AgentRunStatus
	Order           AgentRunOrder
	Limit           int
	Offset          int
}

type AgentRunOrder string

const (
	AgentRunOrderScheduler   AgentRunOrder = "scheduler"
	AgentRunOrderCreatedDesc AgentRunOrder = "created_desc"
)

func sortAgentRuns(runs []*AgentRun, order AgentRunOrder) {
	sort.Slice(runs, func(i, j int) bool {
		if order == AgentRunOrderCreatedDesc {
			if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
				return runs[i].CreatedAt.After(runs[j].CreatedAt)
			}
			return runs[i].ID > runs[j].ID
		}
		if runs[i].Priority != runs[j].Priority {
			return runs[i].Priority > runs[j].Priority
		}
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
}

type AgentRunOwnerSummary struct {
	Owner      ObjectiveOwner `json:"owner"`
	RunCount   int64          `json:"runCount"`
	LastRunAt  *time.Time     `json:"lastRunAt,omitempty"`
	LastStatus AgentRunStatus `json:"lastStatus,omitempty"`
}

type AgentRunSummaryStore interface {
	SummarizeAgentRuns(ctx context.Context, scope Scope, owners []ObjectiveOwner) ([]AgentRunOwnerSummary, error)
}

func summarizeAgentRunOwners(runs []*AgentRun, owners []ObjectiveOwner) []AgentRunOwnerSummary {
	requested := make(map[string]ObjectiveOwner, len(owners))
	for _, owner := range owners {
		requested[string(owner.Type)+"\x1f"+owner.ID] = owner
	}
	summaries := make(map[string]AgentRunOwnerSummary, len(owners))
	for _, run := range runs {
		key := string(run.Owner.Type) + "\x1f" + run.Owner.ID
		owner, ok := requested[key]
		if !ok {
			continue
		}
		summary := summaries[key]
		summary.Owner = owner
		summary.RunCount++
		if summary.LastRunAt == nil || run.CreatedAt.After(*summary.LastRunAt) {
			at := run.CreatedAt
			summary.LastRunAt = &at
			summary.LastStatus = run.Status
		}
		summaries[key] = summary
	}
	result := make([]AgentRunOwnerSummary, 0, len(summaries))
	for _, owner := range owners {
		if summary, ok := summaries[string(owner.Type)+"\x1f"+owner.ID]; ok {
			result = append(result, summary)
		}
	}
	return result
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

type ObjectiveScopeStore interface {
	ListObjectiveScopes(ctx context.Context) ([]Scope, error)
}

// KernelStore is the complete persistence contract required by the OpenSeal
// Engine. Embedding applications can supply an implementation; standalone
// OpenSeal ships memory and SQLite implementations.
type KernelStore interface {
	PortfolioStore
	RunbookActivationStore
	SourceMonitorStore
	ObjectiveScopeStore
	EventSourceCheckpointStore
	EventSourceSubscriptionStore
	RunActivityStore
	ObjectiveActivityStore
	AgentTurnStore
	AgentRunScheduleStore
	ActionStore
}

type CreateObjectiveRequest struct {
	Scope           Scope
	Owner           ObjectiveOwner
	Title           string
	Goal            string
	Status          ObjectiveStatus
	Priority        int
	ExecutionPolicy *ObjectiveExecutionPolicy
	Budget          *BudgetPolicy
	Constraints     map[string]interface{}
	SuccessCriteria map[string]interface{}
	IdempotencyKey  string
	Actor           ActivityActor
	Visibility      ActivityVisibility
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64
	Title            *string
	Goal             *string
	Status           *ObjectiveStatus
	Priority         *int
	ExecutionPolicy  *ObjectiveExecutionPolicy
	Budget           *BudgetPolicy
	Constraints      map[string]interface{}
	SuccessCriteria  map[string]interface{}
	ProgressSummary  *string
	Actor            ActivityActor
	Visibility       ActivityVisibility
	Summary          string
}

type CreateAgentRunRequest struct {
	Scope                Scope
	Kind                 RunKind
	ObjectiveID          string
	ParentRunID          string
	Owner                ObjectiveOwner
	AssignedAgentID      string
	Entrypoint           string
	ConcurrencyKey       string
	ResourceRequirements map[string]int
	Goal                 string
	Source               RunSource
	Priority             int
	Deadline             *time.Time
	AvailableAt          *time.Time
	Context              map[string]interface{}
	Plan                 map[string]interface{}
	Checkpoint           map[string]interface{}
	WakeCondition        *WakeCondition
	Budget               *BudgetPolicy
	Policy               map[string]interface{}
	IdempotencyKey       string
	Actor                ActivityActor
	Visibility           ActivityVisibility
}

type PortfolioService struct {
	store PortfolioStore
	now   func() time.Time
}

type CreateObjectiveResult struct {
	Objective *Objective `json:"objective"`
	Created   bool       `json:"created"`
}

func NewPortfolioService(store PortfolioStore) *PortfolioService {
	return &PortfolioService{store: store, now: time.Now}
}

func (s *PortfolioService) CreateObjective(ctx context.Context, req CreateObjectiveRequest) (*Objective, error) {
	result, err := s.CreateObjectiveIdempotent(ctx, req)
	if err != nil {
		return nil, err
	}
	return result.Objective, nil
}

func (s *PortfolioService) CreateObjectiveIdempotent(ctx context.Context, req CreateObjectiveRequest) (*CreateObjectiveResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	fingerprint, err := objectiveCreationFingerprint(req)
	if err != nil {
		return nil, err
	}
	now := s.now()
	status := req.Status
	if status == "" {
		status = ObjectiveStatusDraft
	}
	objectiveID := uuid.NewString()
	key := strings.TrimSpace(req.IdempotencyKey)
	if len(key) > 256 {
		return nil, fmt.Errorf("%w: idempotency key cannot exceed 256 characters", ErrObjectiveIdempotency)
	}
	if key != "" {
		objectiveID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(req.Scope.Kind+"\x00"+req.Scope.ID+"\x00"+hashString(key))).String()
		current, getErr := s.store.GetObjective(ctx, req.Scope, objectiveID)
		if getErr != nil {
			return nil, getErr
		}
		if current != nil {
			if current.CreationFingerprint != fingerprint {
				return nil, ErrObjectiveIdempotency
			}
			return &CreateObjectiveResult{Objective: current, Created: false}, nil
		}
	}
	objective := &Objective{
		ID: objectiveID, Scope: req.Scope, Owner: req.Owner, Title: req.Title,
		Goal: req.Goal, Status: status, Priority: req.Priority,
		ExecutionPolicy: cloneObjectiveExecutionPolicy(req.ExecutionPolicy), Budget: cloneBudgetPolicy(req.Budget), Constraints: req.Constraints, SuccessCriteria: req.SuccessCriteria,
		Revision: 1, CreatedAt: now, UpdatedAt: now, CreationFingerprint: fingerprint,
	}
	if key != "" {
		objective.IdempotencyKeyHash = hashString(key)
	}
	if err := objective.Validate(); err != nil {
		return nil, err
	}
	persistObjective := func() error {
		activityStore, ok := s.store.(ObjectiveActivityStore)
		if !ok {
			return s.store.CreateObjective(ctx, objective)
		}
		_, persistErr := activityStore.CreateObjectiveWithEvent(ctx, objective, objectiveActivityEvent(objective, req.Actor, req.Visibility, "objective.created", "Objective created", now, map[string]interface{}{"status": objective.Status}))
		return persistErr
	}
	if err := persistObjective(); err != nil {
		if key != "" {
			current, getErr := s.store.GetObjective(ctx, req.Scope, objectiveID)
			if getErr == nil && current != nil {
				if current.CreationFingerprint != fingerprint {
					return nil, ErrObjectiveIdempotency
				}
				return &CreateObjectiveResult{Objective: current, Created: false}, nil
			}
		}
		return nil, err
	}
	return &CreateObjectiveResult{Objective: objective, Created: true}, nil
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
	if req.Status != nil && *req.Status != current.Status && !canTransitionObjective(current.Status, *req.Status) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidObjectiveTransition, current.Status, *req.Status)
	}
	previousStatus := current.Status
	applyObjectiveUpdate(current, req)
	current.UpdatedAt = s.now()
	current.Revision++
	if err := current.Validate(); err != nil {
		return nil, err
	}
	persistUpdate := func() error {
		activityStore, ok := s.store.(ObjectiveActivityStore)
		if !ok {
			return s.store.UpdateObjective(ctx, current, req.ExpectedRevision)
		}
		eventType, summary := "objective.updated", strings.TrimSpace(req.Summary)
		if req.Status != nil && current.Status != previousStatus {
			eventType = "objective.status_changed"
			if summary == "" {
				summary = fmt.Sprintf("Objective changed from %s to %s", previousStatus, current.Status)
			}
		}
		if summary == "" {
			summary = "Objective updated"
		}
		_, persistErr := activityStore.UpdateObjectiveWithEvent(ctx, current, req.ExpectedRevision, objectiveActivityEvent(current, req.Actor, req.Visibility, eventType, summary, current.UpdatedAt, map[string]interface{}{"previousStatus": previousStatus, "status": current.Status, "revision": current.Revision}))
		return persistErr
	}
	if err := persistUpdate(); err != nil {
		return nil, err
	}
	return current, nil
}

func objectiveActivityEvent(objective *Objective, actor ActivityActor, visibility ActivityVisibility, eventType, summary string, occurredAt time.Time, payload map[string]interface{}) *ActivityEvent {
	if strings.TrimSpace(actor.Type) == "" {
		actor = ActivityActor{Type: "system", ID: "openseal"}
	}
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: objective.Scope, ObjectiveID: objective.ID, EventType: eventType,
		Severity: ActivitySeverityInfo, Actor: actor, Summary: summary, Payload: payload,
		Visibility: visibility, CreatedAt: occurredAt,
	}
	if objective.Owner.Type == OwnerTypeAgent {
		event.AgentID = objective.Owner.ID
	} else if objective.Owner.Type == OwnerTypeTeam {
		event.TeamID = objective.Owner.ID
	}
	return event
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
		if req.ParentRunID == "" {
			if err := validateObjectiveRunBudget(objective, req.Budget); err != nil {
				return nil, err
			}
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
	case RunSourceManual, RunSourceChat, RunSourceSchedule, RunSourceEvent, RunSourceWebhook, RunSourceRequest, RunSourceRequestDecision, RunSourceCompletionReview, RunSourceHandoff, RunSourceEscalation, RunSourceObjective, RunSourceFork:
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
		AssignedAgentID: req.AssignedAgentID, Entrypoint: strings.TrimSpace(req.Entrypoint), ConcurrencyKey: strings.TrimSpace(req.ConcurrencyKey),
		ResourceRequirements: cloneResourceQuantities(req.ResourceRequirements),
		Goal:                 req.Goal, Source: source,
		Status: AgentRunStatusQueued, Priority: req.Priority, Deadline: req.Deadline,
		AvailableAt: availableAt, QueueEnteredAt: now, Context: req.Context,
		Plan: req.Plan, Checkpoint: req.Checkpoint, WakeCondition: req.WakeCondition,
		Budget: cloneBudgetPolicy(req.Budget), Policy: req.Policy, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if run.Budget != nil {
		run.BudgetState = BudgetStateActive
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
	case RunKindAgentWork, RunKindConversation, RunKindWorkforceAuthoring:
		return true
	default:
		return false
	}
}

func validateResourceQuantities(values map[string]int, allowZero bool) error {
	if len(values) > 64 {
		return errors.New("resource quantities cannot contain more than 64 entries")
	}
	for name, quantity := range values {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" || trimmed != name || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
			return errors.New("resource names must be trimmed, non-empty, and at most 128 characters")
		}
		if quantity < 0 || !allowZero && quantity == 0 {
			return errors.New("resource quantities must be positive")
		}
		if quantity > 1_000_000_000 {
			return errors.New("resource quantities cannot exceed 1000000000")
		}
	}
	return nil
}

func cloneResourceQuantities(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	cloned := make(map[string]int, len(values))
	for name, quantity := range values {
		cloned[name] = quantity
	}
	return cloned
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
	if filter.Order != "" && filter.Order != AgentRunOrderScheduler && filter.Order != AgentRunOrderCreatedDesc {
		return nil, fmt.Errorf("%w: unsupported run order %q", ErrInvalidAgentRun, filter.Order)
	}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
	}
	return s.store.ListAgentRuns(ctx, filter)
}

func (s *PortfolioService) SummarizeAgentRuns(ctx context.Context, scope Scope, owners []ObjectiveOwner) ([]AgentRunOwnerSummary, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio service is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	for _, owner := range owners {
		if err := owner.Validate(); err != nil {
			return nil, err
		}
	}
	store, ok := s.store.(AgentRunSummaryStore)
	if !ok {
		return nil, errors.New("agent run summaries are unavailable")
	}
	return store.SummarizeAgentRuns(ctx, scope, owners)
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
	if req.ExecutionPolicy != nil {
		objective.ExecutionPolicy = cloneObjectiveExecutionPolicy(req.ExecutionPolicy)
	}
	if req.Budget != nil {
		objective.Budget = cloneBudgetPolicy(req.Budget)
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
}

func validObjectiveStatus(status ObjectiveStatus) bool {
	switch status {
	case ObjectiveStatusDraft, ObjectiveStatusActive, ObjectiveStatusPaused, ObjectiveStatusSatisfied,
		ObjectiveStatusFailed, ObjectiveStatusRetired:
		return true
	default:
		return false
	}
}

func canTransitionObjective(from, to ObjectiveStatus) bool {
	allowed := map[ObjectiveStatus]map[ObjectiveStatus]bool{
		ObjectiveStatusDraft:  {ObjectiveStatusActive: true, ObjectiveStatusRetired: true},
		ObjectiveStatusActive: {ObjectiveStatusPaused: true, ObjectiveStatusSatisfied: true, ObjectiveStatusFailed: true, ObjectiveStatusRetired: true},
		ObjectiveStatusPaused: {ObjectiveStatusActive: true, ObjectiveStatusFailed: true, ObjectiveStatusRetired: true},
	}
	return allowed[from][to]
}

func objectiveCreationFingerprint(req CreateObjectiveRequest) (string, error) {
	payload := struct {
		Scope           Scope                     `json:"scope"`
		Owner           ObjectiveOwner            `json:"owner"`
		Title           string                    `json:"title"`
		Goal            string                    `json:"goal"`
		Status          ObjectiveStatus           `json:"status"`
		Priority        int                       `json:"priority"`
		ExecutionPolicy *ObjectiveExecutionPolicy `json:"executionPolicy,omitempty"`
		Budget          *BudgetPolicy             `json:"budget,omitempty"`
		Constraints     map[string]interface{}    `json:"constraints,omitempty"`
		SuccessCriteria map[string]interface{}    `json:"successCriteria,omitempty"`
	}{req.Scope, req.Owner, req.Title, req.Goal, req.Status, req.Priority, req.ExecutionPolicy,
		req.Budget, req.Constraints, req.SuccessCriteria}
	if payload.Status == "" {
		payload.Status = ObjectiveStatusDraft
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode objective creation fingerprint: %w", err)
	}
	return hashBytes(encoded), nil
}

func (s Scope) key() string { return fmt.Sprintf("%s:%s", s.Kind, s.ID) }
