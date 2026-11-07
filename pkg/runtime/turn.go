package runtime

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

var (
	ErrTurnNotFound     = errors.New("agent turn not found")
	ErrActiveTurnExists = errors.New("an active turn already exists for the run")
	ErrTurnLeaseHeld    = errors.New("agent turn lease is held by another worker")
)

type AgentTurnStatus string

const (
	AgentTurnStatusRunning   AgentTurnStatus = "running"
	AgentTurnStatusCompleted AgentTurnStatus = "completed"
	AgentTurnStatusFailed    AgentTurnStatus = "failed"
	AgentTurnStatusCanceled  AgentTurnStatus = "canceled"
)

type TurnDecision struct {
	Summary      string   `json:"summary"`
	Rationale    string   `json:"rationale,omitempty"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
}

type TurnAction struct {
	Type            string `json:"type"`
	Capability      string `json:"capability,omitempty"`
	BindingID       string `json:"bindingId,omitempty"`
	BindingRevision int64  `json:"bindingRevision,omitempty"`
	// ExpectedSkillVersion is supplied by immutable Runbook execution. Model
	// actions omit it and select the current authorized binding.
	ExpectedSkillVersion string                     `json:"expectedSkillVersion,omitempty"`
	Summary              string                     `json:"summary"`
	IdempotencyKey       string                     `json:"idempotencyKey,omitempty"`
	InputRef             string                     `json:"inputRef,omitempty"`
	EvidenceRefs         []string                   `json:"evidenceRefs,omitempty"`
	ExternalOperation    *ExternalOperationIdentity `json:"externalOperation,omitempty"`
	ReviewContext        *ApprovalReviewContext     `json:"reviewContext,omitempty"`
	// PreparedRuntime is assigned by the trusted worker after the model
	// proposes a capability. Any model-supplied value is discarded before the
	// Turn is persisted.
	PreparedRuntime *skill.PreparedRuntime `json:"preparedRuntime,omitempty"`
}

type TurnUsage struct {
	InputTokens        int     `json:"inputTokens,omitempty"`
	OutputTokens       int     `json:"outputTokens,omitempty"`
	Cost               float64 `json:"cost,omitempty"`
	DurationMS         int64   `json:"durationMs,omitempty"`
	ProviderDurationMS int64   `json:"providerDurationMs,omitempty"`
	RepairAttempts     int     `json:"repairAttempts,omitempty"`
	NativeOperations   int     `json:"nativeOperations,omitempty"`
}

func (u TurnUsage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.DurationMS < 0 || u.ProviderDurationMS < 0 || u.RepairAttempts < 0 || u.NativeOperations < 0 || u.Cost < 0 || math.IsNaN(u.Cost) || math.IsInf(u.Cost, 0) {
		return errors.New("turn usage cannot be negative, NaN, or infinite")
	}
	return nil
}

// StepProducerKind says what produced a step: a model call or a
// deterministic runner (runbook, outreach, capability invocation, agent
// request wrappers).
type StepProducerKind string

const (
	StepProducerModel         StepProducerKind = "model"
	StepProducerDeterministic StepProducerKind = "deterministic"
)

// maximumStepNarrationBytes bounds a step's narration (OutputSummary and the
// turn.narration event summary) on a rune boundary.
const maximumStepNarrationBytes = 2000

// ActivityEventTurnNarration is the public activity event carrying a step's
// narration. It is written in the same transaction as the step's
// OutputSummary and is never posted by connectors.
const ActivityEventTurnNarration = "turn.narration"

// AgentTurn is one step of a Run's loop: at most one model call (or one
// deterministic producer call) and the tool results of that call. It never
// stores model reasoning, tool arguments or tool results; those live in the
// Run's session log.
type AgentTurn struct {
	ID                string                 `json:"id"`
	Scope             Scope                  `json:"scope"`
	RunID             string                 `json:"runId"`
	Sequence          int64                  `json:"sequence"`
	Status            AgentTurnStatus        `json:"status"`
	Producer          StepProducerKind       `json:"producer,omitempty"`
	SessionID         string                 `json:"sessionId,omitempty"`
	AssistantEntryID  string                 `json:"assistantEntryId,omitempty"`
	DefinitionID      string                 `json:"definitionId,omitempty"`
	DefinitionVersion string                 `json:"definitionVersion,omitempty"`
	ModelProvider     string                 `json:"modelProvider,omitempty"`
	Model             string                 `json:"model,omitempty"`
	InputContextRefs  []string               `json:"inputContextRefs,omitempty"`
	SkillSelections   []HostedSkillSelection `json:"skillSelections,omitempty"`
	// OutputSummary is the step's narration: the assistant text of a step
	// that ended in tool calls. Empty for steps without tool calls.
	OutputSummary string `json:"outputSummary,omitempty"`
	ToolCallCount int    `json:"toolCallCount,omitempty"`
	// StopReason is the model stop reason (tool_calls | end | refusal |
	// max_tokens), or "" for deterministic steps.
	StopReason string `json:"stopReason,omitempty"`
	// Final is true when the step's model output had no tool calls or
	// included a final-answer tool call.
	Final           bool             `json:"final,omitempty"`
	Usage           TurnUsage        `json:"usage,omitempty"`
	BudgetAdmission *BudgetAdmission `json:"budgetAdmission,omitempty"`
	// FailureCode is a stable kernel code for a failed or canceled step:
	// host_unavailable, host_configuration, model_context_exceeded,
	// producer_failed, duration_budget, budget_exhausted, run_ended.
	FailureCode    string     `json:"failureCode,omitempty"`
	Error          string     `json:"error,omitempty"`
	LeaseOwner     string     `json:"leaseOwner,omitempty"`
	LeaseExpiresAt *time.Time `json:"leaseExpiresAt,omitempty"`
	Revision       int64      `json:"revision"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	StartedAt      time.Time  `json:"startedAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}

func (t *AgentTurn) Validate() error {
	if t == nil {
		return errors.New("agent turn is required")
	}
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.RunID) == "" {
		return errors.New("agent turn id and run id are required")
	}
	if t.Sequence < 0 || t.Revision < 1 {
		return errors.New("agent turn sequence and revision are invalid")
	}
	if !validAgentTurnStatus(t.Status) {
		return errors.New("agent turn status is invalid")
	}
	if !validStepProducerKind(t.Producer) {
		return errors.New("agent turn producer is invalid")
	}
	if !validStepStopReason(t.StopReason) {
		return errors.New("agent turn stop reason is invalid")
	}
	if t.ToolCallCount < 0 {
		return errors.New("agent turn tool call count cannot be negative")
	}
	if t.BudgetAdmission != nil {
		if err := t.BudgetAdmission.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type AgentTurnFilter struct {
	Scope         Scope
	RunID         string
	Statuses      []AgentTurnStatus
	AfterSequence int64
	// Descending lists newest first (still only rows after AfterSequence).
	Descending bool
	Limit      int
}

type AgentTurnStore interface {
	CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error)
	GetAgentTurn(ctx context.Context, scope Scope, turnID string) (*AgentTurn, error)
	ListAgentTurns(ctx context.Context, filter AgentTurnFilter) ([]*AgentTurn, error)
	UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error
	ClaimAgentTurn(ctx context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error)
	// RecordAgentTurnStepModel writes a running step's model record and
	// appends events in one transaction. turn carries the new revision
	// (expectedRevision+1) and its live lease; the stored row must be running,
	// at expectedRevision and leased to turn.LeaseOwner past turn.UpdatedAt.
	RecordAgentTurnStepModel(ctx context.Context, turn *AgentTurn, expectedRevision int64, events []*ActivityEvent) error
}

type BeginAgentTurnRequest struct {
	Scope             Scope
	RunID             string
	SessionID         string
	Producer          StepProducerKind
	DefinitionID      string
	DefinitionVersion string
	ModelProvider     string
	Model             string
	InputContextRefs  []string
	WorkerID          string
	LeaseDuration     time.Duration
}

// FinishAgentTurnRequest closes a running step. OutputSummary replaces the
// recorded narration only for a step that has no model record (a canceled
// step's reason); a zero Usage keeps the recorded usage.
type FinishAgentTurnRequest struct {
	ExpectedRevision int64
	Status           AgentTurnStatus // completed | failed | canceled
	WorkerID         string
	OutputSummary    string
	Usage            TurnUsage
	BudgetAdmission  *BudgetAdmission
	FailureCode      string
	Error            string
}

// StepModelRecord is written once per step, right after the producer
// returned and before any tool runs, so the activity panel shows the
// narration while tools execute.
type StepModelRecord struct {
	ExpectedRevision int64
	WorkerID         string
	ModelProvider    string
	Model            string
	AssistantEntryID string
	// OutputSummary is the narration: the step's assistant text when it has
	// tool calls, else "". Cut to 2000 bytes on a rune boundary.
	OutputSummary   string
	ToolCallCount   int
	StopReason      string
	Final           bool
	SkillSelections []HostedSkillSelection
	Usage           TurnUsage
}

type AgentTurnService struct {
	portfolio PortfolioStore
	turns     AgentTurnStore
	now       func() time.Time
}

func NewAgentTurnService(portfolio PortfolioStore, turns AgentTurnStore) *AgentTurnService {
	return &AgentTurnService{portfolio: portfolio, turns: turns, now: time.Now}
}

func (s *AgentTurnService) BeginTurn(ctx context.Context, req BeginAgentTurnRequest) (*AgentTurn, error) {
	if s == nil || s.portfolio == nil || s.turns == nil {
		return nil, errors.New("agent turn service is not configured")
	}
	run, err := s.portfolio.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	if run.Status != AgentRunStatusPlanning && run.Status != AgentRunStatusRunning {
		return nil, errors.New("agent run must be planning or running to begin a turn")
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		return nil, errors.New("agent turn worker id is required")
	}
	if !validStepProducerKind(req.Producer) {
		return nil, errors.New("agent turn producer is invalid")
	}
	leaseDuration := req.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = 5 * time.Minute
	}
	now := s.now()
	leaseExpiresAt := now.Add(leaseDuration)
	return s.turns.CreateAgentTurn(ctx, &AgentTurn{
		ID: uuid.NewString(), Scope: req.Scope, RunID: req.RunID, Status: AgentTurnStatusRunning,
		Producer: req.Producer, SessionID: strings.TrimSpace(req.SessionID),
		DefinitionID: req.DefinitionID, DefinitionVersion: req.DefinitionVersion,
		ModelProvider: req.ModelProvider, Model: req.Model, InputContextRefs: req.InputContextRefs,
		LeaseOwner: req.WorkerID, LeaseExpiresAt: &leaseExpiresAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	})
}

func (s *AgentTurnService) FinishTurn(ctx context.Context, scope Scope, turnID string, req FinishAgentTurnRequest) (*AgentTurn, error) {
	if s == nil || s.turns == nil {
		return nil, errors.New("agent turn service is not configured")
	}
	turn, err := s.turns.GetAgentTurn(ctx, scope, turnID)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return nil, ErrTurnNotFound
	}
	if turn.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if turn.Status != AgentTurnStatusRunning || !terminalAgentTurnStatus(req.Status) {
		return nil, errors.New("agent turn can only finish once from running")
	}
	now := s.now()
	if strings.TrimSpace(req.WorkerID) == "" || turn.LeaseOwner != req.WorkerID || turn.LeaseExpiresAt == nil || !turn.LeaseExpiresAt.After(now) {
		return nil, ErrTurnLeaseHeld
	}
	turn.Status = req.Status
	if req.OutputSummary != "" || turn.AssistantEntryID == "" {
		turn.OutputSummary = req.OutputSummary
	}
	if req.Usage != (TurnUsage{}) {
		turn.Usage = req.Usage
	}
	turn.BudgetAdmission = req.BudgetAdmission
	turn.FailureCode = strings.TrimSpace(req.FailureCode)
	turn.Error = req.Error
	turn.LeaseOwner = ""
	turn.LeaseExpiresAt = nil
	turn.CompletedAt = &now
	turn.UpdatedAt = now
	turn.Revision++
	if err := s.turns.UpdateAgentTurn(ctx, turn, req.ExpectedRevision, req.WorkerID); err != nil {
		return nil, err
	}
	return turn, nil
}

// RecordStepModel records a running step's model output (narration, tool
// call count, stop reason, final flag, usage) and, when the step has
// narration, appends the turn.narration activity event in the same store
// transaction. It requires the caller's live lease and ExpectedRevision.
func (s *AgentTurnService) RecordStepModel(ctx context.Context, scope Scope, turnID string, req StepModelRecord) (*AgentTurn, error) {
	if s == nil || s.portfolio == nil || s.turns == nil {
		return nil, errors.New("agent turn service is not configured")
	}
	if strings.TrimSpace(req.WorkerID) == "" {
		return nil, errors.New("agent turn worker id is required")
	}
	if req.ToolCallCount < 0 {
		return nil, errors.New("agent turn tool call count cannot be negative")
	}
	if !validStepStopReason(req.StopReason) {
		return nil, errors.New("agent turn stop reason is invalid")
	}
	if err := req.Usage.Validate(); err != nil {
		return nil, err
	}
	narration := boundStepNarration(req.OutputSummary)
	if narration != "" && req.ToolCallCount == 0 {
		return nil, errors.New("step narration requires tool calls")
	}
	provider, model, err := normalizeModelIdentity(req.ModelProvider, req.Model)
	if err != nil {
		return nil, err
	}
	turn, err := s.turns.GetAgentTurn(ctx, scope, turnID)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return nil, ErrTurnNotFound
	}
	if turn.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	now := s.now()
	if turn.Status != AgentTurnStatusRunning || turn.LeaseOwner != req.WorkerID || turn.LeaseExpiresAt == nil || !turn.LeaseExpiresAt.After(now) {
		return nil, ErrTurnLeaseHeld
	}
	run, err := s.portfolio.GetAgentRun(ctx, scope, turn.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	if provider != "" {
		turn.ModelProvider, turn.Model = provider, model
	}
	turn.AssistantEntryID = strings.TrimSpace(req.AssistantEntryID)
	turn.OutputSummary = narration
	turn.ToolCallCount = req.ToolCallCount
	turn.StopReason = req.StopReason
	turn.Final = req.Final || (turn.Producer != StepProducerDeterministic && req.ToolCallCount == 0)
	turn.SkillSelections = append([]HostedSkillSelection(nil), req.SkillSelections...)
	turn.Usage = req.Usage
	turn.UpdatedAt = now
	turn.Revision++
	var events []*ActivityEvent
	if narration != "" {
		events = append(events, &ActivityEvent{
			ID: uuid.NewString(), Scope: scope, RunID: turn.RunID, TurnID: turn.ID,
			AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: teamIDForRun(run),
			EventType: ActivityEventTurnNarration, Severity: ActivitySeverityInfo,
			Actor:      ActivityActor{Type: "worker", ID: req.WorkerID},
			Summary:    narration,
			Payload:    map[string]interface{}{"stepSequence": turn.Sequence},
			Visibility: ActivityVisibilityScope,
			CreatedAt:  now,
		})
	}
	if err := s.turns.RecordAgentTurnStepModel(ctx, turn, req.ExpectedRevision, events); err != nil {
		return nil, err
	}
	return turn, nil
}

// boundStepNarration trims the narration and cuts it to
// maximumStepNarrationBytes on a rune boundary.
func boundStepNarration(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maximumStepNarrationBytes {
		return text
	}
	cut := maximumStepNarrationBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut])
}

// validateStepModelWrite checks the shared preconditions of
// RecordAgentTurnStepModel against the stored row (status, revision, lease).
func validateStepModelWrite(current, next *AgentTurn, expectedRevision int64) error {
	if current == nil {
		return ErrTurnNotFound
	}
	if current.Revision != expectedRevision || current.Sequence != next.Sequence || current.RunID != next.RunID {
		return ErrRevisionConflict
	}
	if current.Status != AgentTurnStatusRunning || next.Status != AgentTurnStatusRunning ||
		current.LeaseOwner == "" || current.LeaseOwner != next.LeaseOwner ||
		current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(next.UpdatedAt) {
		return ErrTurnLeaseHeld
	}
	return nil
}

func validateStepModelRecordInput(turn *AgentTurn, expectedRevision int64, events []*ActivityEvent) error {
	if err := turn.Validate(); err != nil {
		return err
	}
	if turn.Revision != expectedRevision+1 {
		return ErrRevisionConflict
	}
	if strings.TrimSpace(turn.LeaseOwner) == "" {
		return ErrTurnLeaseHeld
	}
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return err
		}
		if event.Scope != turn.Scope || event.RunID != turn.RunID {
			return ErrInvalidScope
		}
	}
	return nil
}

func normalizeModelIdentity(provider, model string) (string, string, error) {
	provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	if (provider == "") != (model == "") {
		return "", "", errors.New("agent turn model provider and model must be reported together")
	}
	if len(provider) > 256 || len(model) > 256 || strings.ContainsAny(provider, "\r\n\x00") || strings.ContainsAny(model, "\r\n\x00") {
		return "", "", errors.New("agent turn model identity cannot exceed 256 characters or contain control delimiters")
	}
	return provider, model, nil
}

func (s *AgentTurnService) RenewTurn(ctx context.Context, scope Scope, turnID, workerID string, leaseDuration time.Duration) (*AgentTurn, error) {
	if s == nil || s.turns == nil {
		return nil, errors.New("agent turn service is not configured")
	}
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(workerID) == "" {
		return nil, errors.New("turn id and worker id are required")
	}
	if leaseDuration <= 0 {
		leaseDuration = 5 * time.Minute
	}
	turn, err := s.turns.GetAgentTurn(ctx, scope, turnID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if turn.Status != AgentTurnStatusRunning || turn.LeaseOwner != workerID || turn.LeaseExpiresAt == nil || !turn.LeaseExpiresAt.After(now) {
		return nil, ErrTurnLeaseHeld
	}
	expectedRevision := turn.Revision
	expires := now.Add(leaseDuration)
	turn.LeaseExpiresAt = &expires
	turn.UpdatedAt = now
	turn.Revision++
	if err := s.turns.UpdateAgentTurn(ctx, turn, expectedRevision, workerID); err != nil {
		return nil, err
	}
	return turn, nil
}

func (s *AgentTurnService) ReleaseTurn(ctx context.Context, scope Scope, turnID string, expectedRevision int64, workerID string) (*AgentTurn, error) {
	if s == nil || s.turns == nil || strings.TrimSpace(workerID) == "" {
		return nil, errors.New("agent turn service and worker id are required")
	}
	turn, err := s.turns.GetAgentTurn(ctx, scope, turnID)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return nil, ErrTurnNotFound
	}
	now := s.now()
	if turn.Revision != expectedRevision {
		return nil, ErrRevisionConflict
	}
	if turn.Status != AgentTurnStatusRunning || turn.LeaseOwner != workerID || turn.LeaseExpiresAt == nil || !turn.LeaseExpiresAt.After(now) {
		return nil, ErrTurnLeaseHeld
	}
	turn.LeaseOwner, turn.LeaseExpiresAt = "", nil
	turn.UpdatedAt = now
	turn.Revision++
	if err := s.turns.UpdateAgentTurn(ctx, turn, expectedRevision, workerID); err != nil {
		return nil, err
	}
	return turn, nil
}

func (s *AgentTurnService) GetTurn(ctx context.Context, scope Scope, turnID string) (*AgentTurn, error) {
	turn, err := s.turns.GetAgentTurn(ctx, scope, turnID)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return nil, ErrTurnNotFound
	}
	return turn, nil
}

func (s *AgentTurnService) ListTurns(ctx context.Context, filter AgentTurnFilter) ([]*AgentTurn, error) {
	return s.turns.ListAgentTurns(ctx, filter)
}

func (s *AgentTurnService) ClaimTurn(ctx context.Context, scope Scope, turnID, workerID string, leaseDuration time.Duration) (*AgentTurn, error) {
	if s == nil || s.turns == nil {
		return nil, errors.New("agent turn service is not configured")
	}
	if strings.TrimSpace(workerID) == "" {
		return nil, errors.New("agent turn worker id is required")
	}
	if leaseDuration <= 0 {
		leaseDuration = 5 * time.Minute
	}
	return s.turns.ClaimAgentTurn(ctx, scope, turnID, workerID, s.now(), leaseDuration)
}

func validAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnStatusRunning || terminalAgentTurnStatus(status)
}

func validStepProducerKind(kind StepProducerKind) bool {
	return kind == "" || kind == StepProducerModel || kind == StepProducerDeterministic
}

// validStepStopReason accepts the model stop reasons, or "" for
// deterministic and not-yet-recorded steps.
func validStepStopReason(reason string) bool {
	switch reason {
	case "", "tool_calls", "end", "refusal", "max_tokens":
		return true
	}
	return false
}

func terminalAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnStatusCompleted || status == AgentTurnStatusFailed || status == AgentTurnStatusCanceled
}
