package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidScope         = errors.New("scope kind and id are required")
	ErrInvalidOwner         = errors.New("objective owner type and id are required")
	ErrObjectiveNotFound    = errors.New("objective not found")
	ErrObjectiveIdempotency = errors.New("objective idempotency key was already used with different input")
	ErrRunNotFound          = errors.New("run not found")
	ErrRevisionConflict     = errors.New("objective revision conflict")
	ErrRunIdempotency       = errors.New("run idempotency key was already used with different input")
	ErrInvalidAgentRun      = errors.New("invalid agent run")
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
	ID                   string                       `json:"id"`
	Actor                ActivityActor                `json:"actor"`
	Instruction          string                       `json:"instruction"`
	CreatedAt            time.Time                    `json:"createdAt"`
	ConversationAnswer   *ConversationAnswerReceipt   `json:"conversationAnswer,omitempty"`
	SkillSetupResolution *SkillSetupResolutionReceipt `json:"skillSetupResolution,omitempty"`
	// ConversationMessage identifies a channel message delivered to the
	// thread's active foreground conversation Run instead of starting a
	// parallel Run. Instruction carries that message's text.
	ConversationMessage *ConversationMessageDelivery `json:"conversationMessage,omitempty"`
	// ActionCallID is the settled action call (approval card or human
	// intervention) this steering message carries the result of.
	ActionCallID string `json:"actionCallId,omitempty"`
	// AgentTrial is the reply of a try_agent trial child, delivered to the
	// builder Run that waits on it (ActionCallID is the try_agent call).
	AgentTrial *AgentTrialReceipt `json:"agentTrial,omitempty"`
}

// AgentTrialReceipt is how a try_agent trial child ended: Status is
// completed, failed or timed_out; Answer is the reply of a completed trial.
type AgentTrialReceipt struct {
	RunID          string `json:"runId"`
	Status         string `json:"status"`
	Answer         string `json:"answer,omitempty"`
	FailureMessage string `json:"failureMessage,omitempty"`
}

// ConversationAnswerReceipt records the canonical question and authenticated
// reply accepted by the conversation scheduler. Ordinary operator instructions
// never create this receipt. It proves answer provenance, not action approval or
// affirmative consent; the host must interpret the answer for its exact purpose.
type ConversationAnswerReceipt struct {
	Scope                Scope  `json:"scope"`
	RunID                string `json:"runId"`
	AgentID              string `json:"agentId"`
	ConversationTaskID   string `json:"conversationTaskId,omitempty"`
	ConversationID       string `json:"conversationId"`
	SourceRunID          string `json:"sourceRunId"`
	SourceMessageID      string `json:"sourceMessageId"`
	ThreadRootMessageID  string `json:"threadRootMessageId"`
	QuestionMessageID    string `json:"questionMessageId"`
	QuestionTurnID       string `json:"questionTurnId,omitempty"`
	QuestionTurnSequence int64  `json:"questionTurnSequence"`
	QuestionContent      string `json:"questionContent"`
	AnswerMessageID      string `json:"answerMessageId"`
	AnswerContent        string `json:"answerContent"`
	// SelectedOptionIDs are the option ids the answer chose when the question offered options.
	SelectedOptionIDs  []string                `json:"selectedOptionIds,omitempty"`
	AuthenticatedActor ConversationParticipant `json:"authenticatedActor"`
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

// RunCreditError says a Run stopped because its Workspace has no model
// credits. Clients offer the billing step and retry the saved reply instead of
// showing a failure answer, so the kernel posts none to an application chat.
type RunCreditError struct {
	Code string `json:"code"`
}

// AgentRun is the canonical durable workstream. Workflow execution records are
// subordinate execution details and must not be used as agent-run identity.
// RunEngineLoop is AgentRun.Engine for Runs created by the run loop.
const RunEngineLoop = "loop"

type AgentRun struct {
	ID              string         `json:"id"`
	Kind            RunKind        `json:"kind"`
	Scope           Scope          `json:"scope"`
	ObjectiveID     string         `json:"objectiveId,omitempty"`
	ParentRunID     string         `json:"parentRunId,omitempty"`
	RootRunID       string         `json:"rootRunId"`
	Owner           ObjectiveOwner `json:"owner"`
	AssignedAgentID string         `json:"assignedAgentId,omitempty"`
	// ActingUser is the human the run acts for, stamped only at creation by
	// trusted kernel paths and the host. Empty means no person. It is immutable.
	ActingUser           string                 `json:"actingUser,omitempty"`
	Entrypoint           string                 `json:"entrypoint,omitempty"`
	ConcurrencyKey       string                 `json:"concurrencyKey,omitempty"`
	ResourceRequirements map[string]int         `json:"resourceRequirements,omitempty"`
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
	// ConsumedInterventions is how many PendingInterventions the Run's loop
	// has appended to its session. PendingInterventions stays append-only and
	// the cursor only grows; it is written only with the session append.
	ConsumedInterventions int                          `json:"consumedInterventions,omitempty"`
	HumanInterventions    []HumanInterventionRequest   `json:"humanInterventions,omitempty"`
	Budget                *BudgetPolicy                `json:"budget,omitempty"`
	BudgetUsage           BudgetUsage                  `json:"budgetUsage,omitempty"`
	BudgetState           BudgetState                  `json:"budgetState,omitempty"`
	BudgetAdmission       *BudgetAdmission             `json:"budgetAdmission,omitempty"`
	BudgetReservations    map[string]BudgetReservation `json:"budgetReservations,omitempty"`
	BudgetAllocations     map[string]BudgetPolicy      `json:"budgetAllocations,omitempty"`
	Policy                map[string]interface{}       `json:"policy,omitempty"`
	Output                map[string]interface{}       `json:"output,omitempty"`
	Error                 string                       `json:"error,omitempty"`
	CreditError           *RunCreditError              `json:"creditError,omitempty"`
	// FailureExplanation is the kernel-recorded failure of a stopped attempt.
	FailureExplanation  *RunFailureExplanation `json:"failureExplanation,omitempty"`
	WorkflowExecution   *int                   `json:"workflowExecutionId,omitempty"`
	LastAppliedTurn     int64                  `json:"lastAppliedTurn"`
	Revision            int64                  `json:"revision"`
	CreatedAt           time.Time              `json:"createdAt"`
	UpdatedAt           time.Time              `json:"updatedAt"`
	StartedAt           *time.Time             `json:"startedAt,omitempty"`
	CompletedAt         *time.Time             `json:"completedAt,omitempty"`
	IdempotencyKeyHash  string                 `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string                 `json:"creationFingerprint,omitempty"`
	// SessionID is the session the Run's steps append to. It is set when the
	// Run is created and changed only by promotion.
	SessionID string `json:"sessionId,omitempty"`
	// Engine marks the runtime that created the Run; RunEngineLoop for every
	// Run the run loop creates.
	Engine string `json:"engine,omitempty"`
}

// RunFailureExplanation is the failure of a stopped attempt. Only the kernel
// writes it. Explained is true only after a model explanation was posted.
type RunFailureExplanation struct {
	// Kind is execution, workspace, routine_script, task_completion,
	// agent_behavior_fixed, repeat_limit, context_overflow, budget or credits.
	Kind          string `json:"kind"`
	Code          string `json:"code"`
	Message       string `json:"message"` // user-safe cause, at most 1 KiB
	StepError     string `json:"stepError,omitempty"`
	StepErrorCode string `json:"stepErrorCode,omitempty"`
	Explained     bool   `json:"explained,omitempty"`
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
	if !IsValidActingUser(r.ActingUser) {
		return errors.New("run acting user is invalid")
	}
	if _, err := AcceptedRunExecutionForRun(r); err != nil {
		return err
	}
	if strings.TrimSpace(r.Goal) == "" {
		return errors.New("run goal is required")
	}
	if r.Kind != "" && !IsValidRunKind(r.Kind) {
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
	ConcurrencyKey     string // Exact scheduler key; intersected with scope and owner.
	ConversationID     string // Exact canonical conversation ID stored in run context.
	IncludeDescendants bool   // Include work whose scoped root belongs to ConversationID.
	Query              string // Literal case-insensitive substring of the goal or status.
	Scope              Scope
	Kind               RunKind
	Owner              *ObjectiveOwner
	ObjectiveID        string
	ParentRunID        string
	RootRunID          string
	// RootRunIDs matches any listed root, intersected with the other filters.
	RootRunIDs      []string
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

func SortAgentRuns(runs []*AgentRun, order AgentRunOrder) {
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
// OpenSeal ships a SQLite implementation.
type KernelStore interface {
	SkillSetupRequestStore
	CredentialRequestStore
	PortfolioStore
	RunbookActivationStore
	ObjectiveScopeStore
	RunActivityStore
	ObjectiveActivityStore
	AgentTurnStore
	AgentRunScheduleStore
	ActionStore
	AgentSessionStore
	RoutineEventStore
}

type CreateAgentRunRequest struct {
	Scope                Scope
	Kind                 RunKind
	ObjectiveID          string
	ParentRunID          string
	Owner                ObjectiveOwner
	AssignedAgentID      string
	ActingUser           string
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
	// SessionID is the session the new Run appends to.
	SessionID string
}

func NormalizeRunKind(kind RunKind) RunKind {
	if kind == "" {
		return RunKindAgentWork
	}
	return kind
}

func IsValidRunKind(kind RunKind) bool {
	switch kind {
	case RunKindAgentWork, RunKindConversation:
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

func validObjectiveStatus(status ObjectiveStatus) bool {
	switch status {
	case ObjectiveStatusDraft, ObjectiveStatusActive, ObjectiveStatusPaused, ObjectiveStatusSatisfied,
		ObjectiveStatusFailed, ObjectiveStatusRetired:
		return true
	default:
		return false
	}
}

func (s Scope) Key() string { return fmt.Sprintf("%s:%s", s.Kind, s.ID) }

func MatchesObjectiveFilter(objective *Objective, filter ObjectiveFilter) bool {
	if filter.Owner != nil && objective.Owner != *filter.Owner {
		return false
	}
	if !filter.IncludeRetired && objective.Status == ObjectiveStatusRetired {
		return false
	}
	if len(filter.Statuses) > 0 && !containsObjectiveStatus(filter.Statuses, objective.Status) {
		return false
	}
	return true
}

func MatchesRunFilter(run *AgentRun, filter AgentRunFilter) bool {
	if filter.ConcurrencyKey != "" && run.ConcurrencyKey != filter.ConcurrencyKey {
		return false
	}
	if filter.ConversationID != "" && run.Context[ConversationRunConversationIDContextKey] != filter.ConversationID {
		return false
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" && !strings.Contains(strings.ToLower(run.Goal+" "+string(run.Status)), query) {
		return false
	}
	if filter.Kind != "" && NormalizeRunKind(run.Kind) != filter.Kind {
		return false
	}
	if filter.Owner != nil && run.Owner != *filter.Owner {
		return false
	}
	if filter.ObjectiveID != "" && run.ObjectiveID != filter.ObjectiveID {
		return false
	}
	if filter.ParentRunID != "" && run.ParentRunID != filter.ParentRunID {
		return false
	}
	if filter.RootRunID != "" && run.RootRunID != filter.RootRunID {
		return false
	}
	if len(filter.RootRunIDs) > 0 {
		found := false
		for _, rootID := range filter.RootRunIDs {
			if run.RootRunID == rootID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if filter.AssignedAgentID != "" && run.AssignedAgentID != filter.AssignedAgentID {
		return false
	}
	if len(filter.Statuses) > 0 && !containsRunStatus(filter.Statuses, run.Status) {
		return false
	}
	return true
}

func containsObjectiveStatus(statuses []ObjectiveStatus, status ObjectiveStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func containsRunStatus(statuses []AgentRunStatus, status AgentRunStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func CloneObjective(in *Objective) *Objective {
	if in == nil {
		return nil
	}
	var out Objective
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}

func CloneAgentRun(in *AgentRun) *AgentRun {
	if in == nil {
		return nil
	}
	var out AgentRun
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	out.Kind = NormalizeRunKind(out.Kind)
	return &out
}

func SummarizeAgentRunOwners(runs []*AgentRun, owners []ObjectiveOwner) []AgentRunOwnerSummary {
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

func PageObjectives(values []*Objective, offset, limit int) []*Objective {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*Objective{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}

// RunCreditErrorWorkspaceCreditsExhausted is the only RunCreditError code.
const RunCreditErrorWorkspaceCreditsExhausted = "workspace_credits_exhausted"

func ObjectiveActivityEvent(objective *Objective, actor ActivityActor, visibility ActivityVisibility, eventType, summary string, occurredAt time.Time, payload map[string]interface{}) *ActivityEvent {
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
	event.AgentID = ownedItemEventAgentID(objective.Owner, actor)
	if objective.Owner.Type == OwnerTypeTeam {
		event.TeamID = objective.Owner.ID
	}
	return event
}

// ownedItemEventAgentID is the Agent an owned item's activity belongs to: the
// owning Agent, or the Team member that acted on a Team-owned item.
func ownedItemEventAgentID(owner ObjectiveOwner, actor ActivityActor) string {
	switch owner.Type {
	case OwnerTypeAgent:
		return owner.ID
	case OwnerTypeTeam:
		if actor.Type == "agent" {
			return actor.ID
		}
	}
	return ""
}

func CanTransitionObjective(from, to ObjectiveStatus) bool {
	allowed := map[ObjectiveStatus]map[ObjectiveStatus]bool{
		ObjectiveStatusDraft:  {ObjectiveStatusActive: true, ObjectiveStatusRetired: true},
		ObjectiveStatusActive: {ObjectiveStatusPaused: true, ObjectiveStatusSatisfied: true, ObjectiveStatusFailed: true, ObjectiveStatusRetired: true},
		ObjectiveStatusPaused: {ObjectiveStatusActive: true, ObjectiveStatusFailed: true, ObjectiveStatusRetired: true},
	}
	return allowed[from][to]
}

func CloneWakeCondition(condition *WakeCondition) *WakeCondition {
	if condition == nil {
		return nil
	}
	cloned := *condition
	cloned.Predicate = maps.Clone(condition.Predicate)
	if condition.WakeAt != nil {
		wakeAt := *condition.WakeAt
		cloned.WakeAt = &wakeAt
	}
	return &cloned
}
