package runtime

import (
	"context"
	"errors"
	"strings"
)

const HostedTurnAPIVersion = "openseal.hosted-turn/v1"

// HostedSkillPrompt is an immutable, already-authorized prompt projection. It
// contains no binding configuration or credential value.
type HostedSkillPrompt struct {
	SkillID      string `json:"skillId"`
	Version      string `json:"version"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"instructions"`
}

// HostedTurnRequest is the portable execution envelope sent to an Agent host.
// OpenSeal remains authoritative for leases, Turns, actions and state changes;
// the host performs one bounded proposal-only model invocation.
type HostedTurnRequest struct {
	APIVersion             string                 `json:"apiVersion"`
	InvocationID           string                 `json:"invocationId"`
	Scope                  Scope                  `json:"scope"`
	RunID                  string                 `json:"runId"`
	TurnID                 string                 `json:"turnId"`
	AgentID                string                 `json:"agentId"`
	DefinitionID           string                 `json:"definitionId"`
	DefinitionVersion      string                 `json:"definitionVersion"`
	Goal                   string                 `json:"goal"`
	SystemInstructions     []string               `json:"systemInstructions,omitempty"`
	SkillPrompts           []HostedSkillPrompt    `json:"skillPrompts,omitempty"`
	ContinuationCheckpoint map[string]interface{} `json:"continuationCheckpoint,omitempty"`
	PendingInterventions   []AgentRunIntervention `json:"pendingInterventions,omitempty"`
	ModelProvider          string                 `json:"modelProvider,omitempty"`
	Model                  string                 `json:"model,omitempty"`
}

type HostedTurnResponse struct {
	APIVersion             string                 `json:"apiVersion"`
	InvocationID           string                 `json:"invocationId"`
	Decisions              []TurnDecision         `json:"decisions,omitempty"`
	ProposedActions        []TurnAction           `json:"proposedActions,omitempty"`
	OutputSummary          string                 `json:"outputSummary"`
	Usage                  TurnUsage              `json:"usage,omitempty"`
	ContinuationCheckpoint map[string]interface{} `json:"continuationCheckpoint,omitempty"`
	NextRunStatus          AgentRunStatus         `json:"nextRunStatus"`
	WakeCondition          *WakeCondition         `json:"wakeCondition,omitempty"`
	RunOutput              map[string]interface{} `json:"runOutput,omitempty"`
	RunError               string                 `json:"runError,omitempty"`
}

type TurnHost interface {
	ExecuteHostedTurn(context.Context, HostedTurnRequest) (*HostedTurnResponse, error)
}

type HostedTurnRunnerConfig struct {
	AgentID            string
	DefinitionID       string
	DefinitionVersion  string
	SystemInstructions []string
	SkillPrompts       []HostedSkillPrompt
	ModelProvider      string
	Model              string
}

type HostedTurnRunner struct {
	host   TurnHost
	config HostedTurnRunnerConfig
}

func NewHostedTurnRunner(host TurnHost, config HostedTurnRunnerConfig) (*HostedTurnRunner, error) {
	if host == nil || strings.TrimSpace(config.AgentID) == "" || strings.TrimSpace(config.DefinitionID) == "" || strings.TrimSpace(config.DefinitionVersion) == "" {
		return nil, errors.New("turn host and Agent definition identity are required")
	}
	for _, prompt := range config.SkillPrompts {
		if strings.TrimSpace(prompt.SkillID) == "" || strings.TrimSpace(prompt.Version) == "" || strings.TrimSpace(prompt.Instructions) == "" {
			return nil, errors.New("hosted Skill prompts require identity, version, and instructions")
		}
	}
	return &HostedTurnRunner{host: host, config: config}, nil
}

func (r *HostedTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || r.host == nil || input.Run == nil || input.Turn == nil {
		return nil, errors.New("hosted turn requires a durable Run and Turn")
	}
	request := HostedTurnRequest{
		APIVersion: HostedTurnAPIVersion, InvocationID: input.Turn.ID,
		Scope: input.Run.Scope, RunID: input.Run.ID, TurnID: input.Turn.ID,
		AgentID: r.config.AgentID, DefinitionID: r.config.DefinitionID, DefinitionVersion: r.config.DefinitionVersion,
		Goal: input.Run.Goal, SystemInstructions: append([]string(nil), r.config.SystemInstructions...),
		SkillPrompts:           cloneHostedSkillPrompts(r.config.SkillPrompts),
		ContinuationCheckpoint: cloneMap(input.Run.Checkpoint),
		PendingInterventions:   append([]AgentRunIntervention(nil), input.Run.PendingInterventions...),
		ModelProvider:          r.config.ModelProvider, Model: r.config.Model,
	}
	response, err := r.host.ExecuteHostedTurn(ctx, request)
	if err != nil {
		return nil, err
	}
	if response == nil || response.APIVersion != HostedTurnAPIVersion || response.InvocationID != input.Turn.ID {
		return nil, errors.New("turn host returned a mismatched response envelope")
	}
	if err := response.Usage.Validate(); err != nil {
		return nil, err
	}
	return &TurnOutcome{
		Decisions: append([]TurnDecision(nil), response.Decisions...), ProposedActions: append([]TurnAction(nil), response.ProposedActions...),
		OutputSummary: response.OutputSummary, Usage: response.Usage,
		ContinuationCheckpoint: cloneMap(response.ContinuationCheckpoint), NextRunStatus: response.NextRunStatus,
		WakeCondition: cloneWakeCondition(response.WakeCondition), RunOutput: cloneMap(response.RunOutput), RunError: response.RunError,
	}, nil
}

func cloneHostedSkillPrompts(values []HostedSkillPrompt) []HostedSkillPrompt {
	if values == nil {
		return nil
	}
	return append([]HostedSkillPrompt(nil), values...)
}
