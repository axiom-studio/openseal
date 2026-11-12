package kernelapi

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// AgentTurnRecord is the public projection of one model step (agent_turns
// row), agent-turns v2. It never contains reasoning blocks, tool arguments,
// tool results, session entries, Run checkpoints, leases or provider payloads.
type AgentTurnRecord struct {
	ID        string       `json:"id"`
	Scope     models.Scope `json:"scope"`
	RunID     string       `json:"runId"`
	SessionID string       `json:"sessionId,omitempty"`
	// Sequence is the step number within the Run, from 1.
	Sequence          int64                   `json:"sequence"`
	Status            models.AgentTurnStatus  `json:"status"`
	Producer          models.StepProducerKind `json:"producer,omitempty"`
	DefinitionID      string                  `json:"definitionId,omitempty"`
	DefinitionVersion string                  `json:"definitionVersion,omitempty"`
	ModelProvider     string                  `json:"modelProvider,omitempty"`
	Model             string                  `json:"model,omitempty"`
	// OutputSummary is the step narration (D11): the assistant text of a
	// step that ended in tool calls.
	OutputSummary string `json:"outputSummary,omitempty"`
	ToolCallCount int    `json:"toolCallCount"`
	// StopReason is a runtime.ModelStopReason value, or "" for
	// deterministic steps.
	StopReason string `json:"stopReason,omitempty"`
	// Final is true when the step's output had no tool calls or included a
	// final-answer tool call.
	Final       bool             `json:"final"`
	Usage       models.TurnUsage `json:"usage,omitempty"`
	FailureCode string           `json:"failureCode,omitempty"`
	Error       string           `json:"error,omitempty"`
	Revision    int64            `json:"revision"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
	StartedAt   time.Time        `json:"startedAt"`
	CompletedAt *time.Time       `json:"completedAt,omitempty"`
}

func ProjectAgentTurn(turn *models.AgentTurn) *AgentTurnRecord {
	if turn == nil {
		return nil
	}
	return &AgentTurnRecord{
		ID: turn.ID, Scope: turn.Scope, RunID: turn.RunID, SessionID: turn.SessionID,
		Sequence: turn.Sequence, Status: turn.Status, Producer: turn.Producer,
		DefinitionID: turn.DefinitionID, DefinitionVersion: turn.DefinitionVersion,
		ModelProvider: turn.ModelProvider, Model: turn.Model,
		OutputSummary: turn.OutputSummary, ToolCallCount: turn.ToolCallCount,
		StopReason: turn.StopReason, Final: turn.Final, Usage: turn.Usage,
		FailureCode: turn.FailureCode, Error: turn.Error, Revision: turn.Revision,
		CreatedAt: turn.CreatedAt, UpdatedAt: turn.UpdatedAt, StartedAt: turn.StartedAt,
		CompletedAt: cloneTime(turn.CompletedAt),
	}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
