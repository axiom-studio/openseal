package runtime

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

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
	Type            string   `json:"type"`
	Capability      string   `json:"capability,omitempty"`
	BindingID       string   `json:"bindingId,omitempty"`
	BindingRevision int64    `json:"bindingRevision,omitempty"`
	Summary         string   `json:"summary"`
	IdempotencyKey  string   `json:"idempotencyKey,omitempty"`
	InputRef        string   `json:"inputRef,omitempty"`
	EvidenceRefs    []string `json:"evidenceRefs,omitempty"`
	// PreparedRuntime is assigned by the trusted worker after the model
	// proposes a capability. Any model-supplied value is discarded before the
	// Turn is persisted.
	PreparedRuntime *skill.PreparedRuntime `json:"preparedRuntime,omitempty"`
}

type TurnUsage struct {
	InputTokens  int     `json:"inputTokens,omitempty"`
	OutputTokens int     `json:"outputTokens,omitempty"`
	Cost         float64 `json:"cost,omitempty"`
	DurationMS   int64   `json:"durationMs,omitempty"`
}

func (u TurnUsage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.DurationMS < 0 || u.Cost < 0 || math.IsNaN(u.Cost) || math.IsInf(u.Cost, 0) {
		return errors.New("turn usage cannot be negative, NaN, or infinite")
	}
	return nil
}

// AgentTurn is one bounded, resumable reasoning step. It records concise
// operator-facing decisions and references, never private model chain-of-thought
// or resolved secret values.
type AgentTurn struct {
	ID                     string                  `json:"id"`
	Scope                  Scope                   `json:"scope"`
	RunID                  string                  `json:"runId"`
	Sequence               int64                   `json:"sequence"`
	Status                 AgentTurnStatus         `json:"status"`
	DefinitionID           string                  `json:"definitionId,omitempty"`
	DefinitionVersion      string                  `json:"definitionVersion,omitempty"`
	ModelProvider          string                  `json:"modelProvider,omitempty"`
	Model                  string                  `json:"model,omitempty"`
	InputContextRefs       []string                `json:"inputContextRefs,omitempty"`
	PlanRevision           int64                   `json:"planRevision,omitempty"`
	SkillSelections        []HostedSkillSelection  `json:"skillSelections,omitempty"`
	Decisions              []TurnDecision          `json:"decisions,omitempty"`
	RequestedActions       []TurnAction            `json:"requestedActions,omitempty"`
	RequestedFork          *TurnForkProposal       `json:"requestedFork,omitempty"`
	RequestedDelegation    *TurnDelegationProposal `json:"requestedDelegation,omitempty"`
	OutputSummary          string                  `json:"outputSummary,omitempty"`
	Usage                  TurnUsage               `json:"usage,omitempty"`
	ContinuationCheckpoint map[string]interface{}  `json:"continuationCheckpoint,omitempty"`
	NextRunStatus          AgentRunStatus          `json:"nextRunStatus,omitempty"`
	WakeCondition          *WakeCondition          `json:"wakeCondition,omitempty"`
	RunOutput              map[string]interface{}  `json:"runOutput,omitempty"`
	RunError               string                  `json:"runError,omitempty"`
	Error                  string                  `json:"error,omitempty"`
	LeaseOwner             string                  `json:"leaseOwner,omitempty"`
	LeaseExpiresAt         *time.Time              `json:"leaseExpiresAt,omitempty"`
	Revision               int64                   `json:"revision"`
	CreatedAt              time.Time               `json:"createdAt"`
	UpdatedAt              time.Time               `json:"updatedAt"`
	StartedAt              time.Time               `json:"startedAt"`
	CompletedAt            *time.Time              `json:"completedAt,omitempty"`
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
	return nil
}

type AgentTurnFilter struct {
	Scope         Scope
	RunID         string
	AfterSequence int64
	Limit         int
}

type AgentTurnStore interface {
	CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error)
	GetAgentTurn(ctx context.Context, scope Scope, turnID string) (*AgentTurn, error)
	ListAgentTurns(ctx context.Context, filter AgentTurnFilter) ([]*AgentTurn, error)
	UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error
	ClaimAgentTurn(ctx context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error)
}

type BeginAgentTurnRequest struct {
	Scope             Scope
	RunID             string
	DefinitionID      string
	DefinitionVersion string
	ModelProvider     string
	Model             string
	InputContextRefs  []string
	PlanRevision      int64
	WorkerID          string
	LeaseDuration     time.Duration
}

type FinishAgentTurnRequest struct {
	ExpectedRevision       int64
	Status                 AgentTurnStatus
	ModelProvider          string
	Model                  string
	SkillSelections        []HostedSkillSelection
	Decisions              []TurnDecision
	RequestedActions       []TurnAction
	RequestedFork          *TurnForkProposal
	RequestedDelegation    *TurnDelegationProposal
	OutputSummary          string
	Usage                  TurnUsage
	ContinuationCheckpoint map[string]interface{}
	NextRunStatus          AgentRunStatus
	WakeCondition          *WakeCondition
	RunOutput              map[string]interface{}
	RunError               string
	Error                  string
	WorkerID               string
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
	leaseDuration := req.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = 5 * time.Minute
	}
	now := s.now()
	leaseExpiresAt := now.Add(leaseDuration)
	return s.turns.CreateAgentTurn(ctx, &AgentTurn{
		ID: uuid.NewString(), Scope: req.Scope, RunID: req.RunID, Status: AgentTurnStatusRunning,
		DefinitionID: req.DefinitionID, DefinitionVersion: req.DefinitionVersion,
		ModelProvider: req.ModelProvider, Model: req.Model, InputContextRefs: req.InputContextRefs,
		PlanRevision: req.PlanRevision, LeaseOwner: req.WorkerID, LeaseExpiresAt: &leaseExpiresAt,
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
	req.ModelProvider, req.Model, err = normalizeModelIdentity(req.ModelProvider, req.Model)
	if err != nil {
		return nil, err
	}
	if req.ModelProvider != "" {
		turn.ModelProvider = req.ModelProvider
		turn.Model = req.Model
	}
	if req.NextRunStatus == "" {
		switch req.Status {
		case AgentTurnStatusCompleted:
			req.NextRunStatus = AgentRunStatusRunning
		case AgentTurnStatusFailed:
			req.NextRunStatus = AgentRunStatusFailed
		case AgentTurnStatusCanceled:
			req.NextRunStatus = AgentRunStatusCanceled
		}
	}
	if isWaitingRunStatus(req.NextRunStatus) && req.WakeCondition == nil {
		return nil, errors.New("waiting turn outcome requires a wake condition")
	}
	turn.SkillSelections = append([]HostedSkillSelection(nil), req.SkillSelections...)
	turn.Decisions = req.Decisions
	turn.RequestedActions = req.RequestedActions
	turn.RequestedFork = req.RequestedFork
	turn.RequestedDelegation = req.RequestedDelegation
	turn.OutputSummary = req.OutputSummary
	turn.Usage = req.Usage
	turn.ContinuationCheckpoint = req.ContinuationCheckpoint
	turn.NextRunStatus = req.NextRunStatus
	turn.WakeCondition = req.WakeCondition
	turn.RunOutput = req.RunOutput
	turn.RunError = req.RunError
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

func terminalAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnStatusCompleted || status == AgentTurnStatusFailed || status == AgentTurnStatusCanceled
}
