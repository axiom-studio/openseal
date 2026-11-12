package models

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
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

type TurnUsage struct {
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
	// CacheReadTokens and CacheWriteTokens are the prompt-cache part of
	// InputTokens as the provider reported it: the regression alarm for
	// prompt-cache stability.
	CacheReadTokens    int     `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens   int     `json:"cacheWriteTokens,omitempty"`
	Cost               float64 `json:"cost,omitempty"`
	DurationMS         int64   `json:"durationMs,omitempty"`
	ProviderDurationMS int64   `json:"providerDurationMs,omitempty"`
	RepairAttempts     int     `json:"repairAttempts,omitempty"`
	NativeOperations   int     `json:"nativeOperations,omitempty"`
}

func (u TurnUsage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.DurationMS < 0 || u.ProviderDurationMS < 0 || u.RepairAttempts < 0 || u.NativeOperations < 0 || u.Cost < 0 || math.IsNaN(u.Cost) || math.IsInf(u.Cost, 0) {
		return errors.New("turn usage cannot be negative, NaN, or infinite")
	}
	return nil
}

// StepProducerKind says what produced a step: a model call or a
// deterministic runner (runbook, capability invocation, agent
// request wrappers).
type StepProducerKind string

const (
	StepProducerModel         StepProducerKind = "model"
	StepProducerDeterministic StepProducerKind = "deterministic"
)

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
	if !IsValidStepProducerKind(t.Producer) {
		return errors.New("agent turn producer is invalid")
	}
	if !IsValidStepStopReason(t.StopReason) {
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

// ValidateStepModelWrite checks the shared preconditions of
// RecordAgentTurnStepModel against the stored row (status, revision, lease).
func ValidateStepModelWrite(current, next *AgentTurn, expectedRevision int64) error {
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

func ValidateStepModelRecordInput(turn *AgentTurn, expectedRevision int64, events []*ActivityEvent) error {
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

func validAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnStatusRunning || IsTerminalAgentTurnStatus(status)
}

func IsValidStepProducerKind(kind StepProducerKind) bool {
	return kind == "" || kind == StepProducerModel || kind == StepProducerDeterministic
}

// IsValidStepStopReason accepts the model stop reasons, or "" for
// deterministic and not-yet-recorded steps.
func IsValidStepStopReason(reason string) bool {
	switch reason {
	case "", "tool_calls", "end", "refusal", "max_tokens":
		return true
	}
	return false
}

func IsTerminalAgentTurnStatus(status AgentTurnStatus) bool {
	return status == AgentTurnStatusCompleted || status == AgentTurnStatusFailed || status == AgentTurnStatusCanceled
}

func CloneAgentTurn(in *AgentTurn) *AgentTurn {
	if in == nil {
		return nil
	}
	var out AgentTurn
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}
