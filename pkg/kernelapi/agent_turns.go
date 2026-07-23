package kernelapi

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// AgentTurnRecord is the public, operator-facing projection of one durable
// bounded turn. It intentionally excludes continuation checkpoints, model
// input context, prepared runtimes, action inputs, run output, leases, and
// private provider payloads.
type AgentTurnRecord struct {
	ID                string                         `json:"id"`
	Scope             runtime.Scope                  `json:"scope"`
	RunID             string                         `json:"runId"`
	Sequence          int64                          `json:"sequence"`
	Status            runtime.AgentTurnStatus        `json:"status"`
	DefinitionID      string                         `json:"definitionId,omitempty"`
	DefinitionVersion string                         `json:"definitionVersion,omitempty"`
	ModelProvider     string                         `json:"modelProvider,omitempty"`
	Model             string                         `json:"model,omitempty"`
	PlanRevision      int64                          `json:"planRevision,omitempty"`
	SkillSelections   []runtime.HostedSkillSelection `json:"skillSelections,omitempty"`
	Decisions         []AgentTurnDecisionRecord      `json:"decisions,omitempty"`
	RequestedActions  []AgentTurnActionRecord        `json:"requestedActions,omitempty"`
	OutputSummary     string                         `json:"outputSummary,omitempty"`
	Usage             runtime.TurnUsage              `json:"usage,omitempty"`
	NextRunStatus     runtime.AgentRunStatus         `json:"nextRunStatus,omitempty"`
	WakeCondition     *runtime.WakeCondition         `json:"wakeCondition,omitempty"`
	RunError          string                         `json:"runError,omitempty"`
	Error             string                         `json:"error,omitempty"`
	Revision          int64                          `json:"revision"`
	CreatedAt         time.Time                      `json:"createdAt"`
	UpdatedAt         time.Time                      `json:"updatedAt"`
	StartedAt         time.Time                      `json:"startedAt"`
	CompletedAt       *time.Time                     `json:"completedAt,omitempty"`
}

type AgentTurnDecisionRecord struct {
	Summary      string   `json:"summary"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
}

type AgentTurnActionRecord struct {
	Type            string   `json:"type"`
	Capability      string   `json:"capability,omitempty"`
	BindingID       string   `json:"bindingId,omitempty"`
	BindingRevision int64    `json:"bindingRevision,omitempty"`
	Summary         string   `json:"summary"`
	EvidenceRefs    []string `json:"evidenceRefs,omitempty"`
}

func ProjectAgentTurn(turn *runtime.AgentTurn) *AgentTurnRecord {
	if turn == nil {
		return nil
	}
	record := &AgentTurnRecord{
		ID: turn.ID, Scope: turn.Scope, RunID: turn.RunID, Sequence: turn.Sequence, Status: turn.Status,
		DefinitionID: turn.DefinitionID, DefinitionVersion: turn.DefinitionVersion,
		ModelProvider: turn.ModelProvider, Model: turn.Model, PlanRevision: turn.PlanRevision,
		SkillSelections: append([]runtime.HostedSkillSelection(nil), turn.SkillSelections...),
		OutputSummary:   turn.OutputSummary, Usage: turn.Usage, NextRunStatus: turn.NextRunStatus,
		WakeCondition: cloneWakeCondition(turn.WakeCondition), RunError: turn.RunError, Error: turn.Error,
		Revision: turn.Revision, CreatedAt: turn.CreatedAt, UpdatedAt: turn.UpdatedAt,
		StartedAt: turn.StartedAt, CompletedAt: cloneTime(turn.CompletedAt),
	}
	for _, decision := range turn.Decisions {
		record.Decisions = append(record.Decisions, AgentTurnDecisionRecord{
			Summary: decision.Summary, EvidenceRefs: append([]string(nil), decision.EvidenceRefs...),
		})
	}
	for _, action := range turn.RequestedActions {
		record.RequestedActions = append(record.RequestedActions, AgentTurnActionRecord{
			Type: action.Type, Capability: action.Capability, BindingID: action.BindingID,
			BindingRevision: action.BindingRevision, Summary: action.Summary,
			EvidenceRefs: append([]string(nil), action.EvidenceRefs...),
		})
	}
	return record
}

func cloneWakeCondition(value *runtime.WakeCondition) *runtime.WakeCondition {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
