package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	// HostedTurnProtocolInputReserveTokens covers the fixed host protocol,
	// message framing, and provider-tokenizer uncertainty. Dynamic model input
	// is then charged at one token per UTF-8 byte, a deliberately conservative
	// upper bound for byte-level OpenAI-compatible tokenizers.
	HostedTurnProtocolInputReserveTokens int64 = 4096
	HostedTurnMinimumOutputTokens        int64 = 64
)

// HostedTurnModelInput is the credential-free data envelope presented to the
// model. Keeping it portable lets the kernel reserve the same immutable input
// that an enterprise host dispatches.
type HostedTurnModelInput struct {
	Goal                   string                   `json:"goal"`
	InputContext           map[string]interface{}   `json:"inputContext,omitempty"`
	SystemInstructions     []string                 `json:"systemInstructions,omitempty"`
	SkillPrompts           []HostedSkillPrompt      `json:"skillPrompts,omitempty"`
	Actions                []capability.ModelAction `json:"actions,omitempty"`
	Budget                 *HostedRunBudget         `json:"budget,omitempty"`
	DependencyResults      map[string]interface{}   `json:"dependencyResults,omitempty"`
	ContinuationCheckpoint map[string]interface{}   `json:"continuationCheckpoint,omitempty"`
	PendingInterventions   []AgentRunIntervention   `json:"pendingInterventions,omitempty"`
}

func MarshalHostedTurnModelInput(request HostedTurnRequest) ([]byte, error) {
	return json.Marshal(HostedTurnModelInput{
		Goal: request.Goal, InputContext: request.InputContext, SystemInstructions: request.SystemInstructions,
		SkillPrompts: request.SkillPrompts, Actions: request.Actions, Budget: request.Budget,
		DependencyResults: request.DependencyResults, ContinuationCheckpoint: request.ContinuationCheckpoint,
		PendingInterventions: request.PendingInterventions,
	})
}

func EstimateHostedTurnInputTokens(request HostedTurnRequest) (int64, error) {
	input, err := MarshalHostedTurnModelInput(request)
	if err != nil {
		return 0, err
	}
	return HostedTurnProtocolInputReserveTokens + int64(len(input)), nil
}

func (r *HostedTurnRunner) PlanTurnBudget(_ context.Context, input TurnExecutionContext) (BudgetUsage, error) {
	if r == nil || input.Run == nil || input.Turn == nil || input.Run.Budget == nil {
		return BudgetUsage{}, nil
	}
	request, err := r.buildRequest(input)
	if err != nil {
		return BudgetUsage{}, err
	}
	estimatedInput, err := EstimateHostedTurnInputTokens(request)
	if err != nil {
		return BudgetUsage{}, fmt.Errorf("estimate hosted Turn input: %w", err)
	}
	policy := request.Budget.Policy
	remaining := request.Budget.Remaining
	reservation := BudgetUsage{}
	if policy.MaxInputTokens > 0 {
		if remaining.MaxInputTokens < estimatedInput {
			return BudgetUsage{}, fmt.Errorf("%w: hosted input requires %d tokens but %d remain", ErrBudgetExhausted, estimatedInput, remaining.MaxInputTokens)
		}
		reservation.InputTokens = estimatedInput
	}
	if policy.MaxTotalTokens > 0 {
		if remaining.MaxTotalTokens < estimatedInput+HostedTurnMinimumOutputTokens {
			return BudgetUsage{}, fmt.Errorf("%w: hosted input requires %d tokens plus %d minimum output tokens but %d total tokens remain", ErrBudgetExhausted, estimatedInput, HostedTurnMinimumOutputTokens, remaining.MaxTotalTokens)
		}
		reservation.InputTokens = estimatedInput
		reservation.OutputTokens = remaining.MaxTotalTokens - estimatedInput
	}
	if policy.MaxOutputTokens > 0 {
		if remaining.MaxOutputTokens < HostedTurnMinimumOutputTokens {
			return BudgetUsage{}, fmt.Errorf("%w: hosted output requires at least %d tokens but %d remain", ErrBudgetExhausted, HostedTurnMinimumOutputTokens, remaining.MaxOutputTokens)
		}
		if reservation.OutputTokens == 0 || remaining.MaxOutputTokens < reservation.OutputTokens {
			reservation.OutputTokens = remaining.MaxOutputTokens
		}
	}
	if reservation.OutputTokens > 0 && reservation.OutputTokens < HostedTurnMinimumOutputTokens {
		return BudgetUsage{}, errors.New("hosted output reservation is below the portable minimum")
	}
	return reservation, nil
}
