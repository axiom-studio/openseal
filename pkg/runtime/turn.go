package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

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

// ActivityEventTurnNarration is the public activity event carrying a step's
// narration. It is written in the same transaction as the step's
// OutputSummary and is never posted by connectors.
const ActivityEventTurnNarration = "turn.narration"

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
