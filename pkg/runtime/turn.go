package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrTurnNotFound     = errors.New("agent turn not found")
	ErrActiveTurnExists = errors.New("an active turn already exists for the run")
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
	Type           string `json:"type"`
	Capability     string `json:"capability,omitempty"`
	Summary        string `json:"summary"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	InputRef       string `json:"inputRef,omitempty"`
}

type TurnUsage struct {
	InputTokens  int     `json:"inputTokens,omitempty"`
	OutputTokens int     `json:"outputTokens,omitempty"`
	Cost         float64 `json:"cost,omitempty"`
	DurationMS   int64   `json:"durationMs,omitempty"`
}

// AgentTurn is one bounded, resumable reasoning step. It records concise
// operator-facing decisions and references, never private model chain-of-thought
// or resolved secret values.
type AgentTurn struct {
	ID                     string                 `json:"id"`
	Scope                  Scope                  `json:"scope"`
	RunID                  string                 `json:"runId"`
	Sequence               int64                  `json:"sequence"`
	Status                 AgentTurnStatus        `json:"status"`
	DefinitionID           string                 `json:"definitionId,omitempty"`
	DefinitionVersion      string                 `json:"definitionVersion,omitempty"`
	ModelProvider          string                 `json:"modelProvider,omitempty"`
	Model                  string                 `json:"model,omitempty"`
	InputContextRefs       []string               `json:"inputContextRefs,omitempty"`
	PlanRevision           int64                  `json:"planRevision,omitempty"`
	Decisions              []TurnDecision         `json:"decisions,omitempty"`
	RequestedActions       []TurnAction           `json:"requestedActions,omitempty"`
	OutputSummary          string                 `json:"outputSummary,omitempty"`
	Usage                  TurnUsage              `json:"usage,omitempty"`
	ContinuationCheckpoint map[string]interface{} `json:"continuationCheckpoint,omitempty"`
	Error                  string                 `json:"error,omitempty"`
	Revision               int64                  `json:"revision"`
	CreatedAt              time.Time              `json:"createdAt"`
	UpdatedAt              time.Time              `json:"updatedAt"`
	StartedAt              time.Time              `json:"startedAt"`
	CompletedAt            *time.Time             `json:"completedAt,omitempty"`
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
	UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64) error
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
}

type FinishAgentTurnRequest struct {
	ExpectedRevision       int64
	Status                 AgentTurnStatus
	Decisions              []TurnDecision
	RequestedActions       []TurnAction
	OutputSummary          string
	Usage                  TurnUsage
	ContinuationCheckpoint map[string]interface{}
	Error                  string
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
	now := s.now()
	return s.turns.CreateAgentTurn(ctx, &AgentTurn{
		ID: uuid.NewString(), Scope: req.Scope, RunID: req.RunID, Status: AgentTurnStatusRunning,
		DefinitionID: req.DefinitionID, DefinitionVersion: req.DefinitionVersion,
		ModelProvider: req.ModelProvider, Model: req.Model, InputContextRefs: req.InputContextRefs,
		PlanRevision: req.PlanRevision, Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
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
	turn.Status = req.Status
	turn.Decisions = req.Decisions
	turn.RequestedActions = req.RequestedActions
	turn.OutputSummary = req.OutputSummary
	turn.Usage = req.Usage
	turn.ContinuationCheckpoint = req.ContinuationCheckpoint
	turn.Error = req.Error
	turn.CompletedAt = &now
	turn.UpdatedAt = now
	turn.Revision++
	if err := s.turns.UpdateAgentTurn(ctx, turn, req.ExpectedRevision); err != nil {
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

func validAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnStatusRunning || terminalAgentTurnStatus(status)
}

func terminalAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnStatusCompleted || status == AgentTurnStatusFailed || status == AgentTurnStatusCanceled
}
