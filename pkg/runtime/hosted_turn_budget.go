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
	// message framing, and provider-tokenizer uncertainty. Dynamic JSON is
	// estimated separately at two UTF-8 bytes per token. Agent context must use
	// references rather than embedding opaque binary or encoded artifact data.
	// The reserve includes the host's provider-facing tool framing, which is
	// deliberately absent from the portable model-input JSON below.
	HostedTurnProtocolInputReserveTokens int64 = 6144
	// HostedTurnBudgetEnvelopeReserveTokens covers the bounded JSON growth when
	// the durable reservation is projected into the model-visible budget after
	// preflight. EstimateHostedTurnInputTokens intentionally normalizes that
	// self-referential field so kernel and host calculate one stable estimate.
	HostedTurnBudgetEnvelopeReserveTokens int64 = 256
	// HostedTurnInputSettlementToleranceTokens covers bounded provider
	// tokenizer drift after the kernel reserves its immutable envelope. The
	// exact usage is still charged and must fit the user's remaining budget.
	HostedTurnInputSettlementToleranceTokens int64 = 256
	HostedTurnMinimumOutputTokens            int64 = 64
	// Hosted child work must be large enough to carry at least one complete
	// provider-neutral hosted protocol exchange with room for bounded recovery.
	// These are protocol floors, not domain-specific recommendations; a model
	// may allocate more when the parent has capacity and the work requires it.
	HostedTurnMinimumChildAttempts     int64 = 3
	HostedTurnMinimumChildTurns        int64 = 2
	HostedTurnMinimumChildInputTokens  int64 = 16384
	HostedTurnMinimumChildOutputTokens int64 = 4096
	HostedTurnMinimumChildTotalTokens  int64 = HostedTurnMinimumChildInputTokens + HostedTurnMinimumChildOutputTokens
	HostedTurnMinimumChildDurationMS   int64 = 180000
	HostedTurnMinimumChildActions      int64 = 1
)

// HostedTurnModelInput is the credential-free data envelope presented to the
// model. Keeping it portable lets the kernel reserve the same immutable input
// that an enterprise host dispatches.
type HostedTurnModelInput struct {
	Goal                   string                   `json:"goal"`
	InputContext           map[string]interface{}   `json:"inputContext,omitempty"`
	SystemInstructions     []string                 `json:"systemInstructions,omitempty"`
	EligibleAgents         []HostedAgentTarget      `json:"eligibleAgents,omitempty"`
	RunbookOperations      []HostedRunbookOperation `json:"runbookOperations,omitempty"`
	SkillPrompts           []HostedSkillPrompt      `json:"skillPrompts,omitempty"`
	Actions                []capability.ModelAction `json:"actions,omitempty"`
	Budget                 *HostedRunBudget         `json:"budget,omitempty"`
	DependencyResults      map[string]interface{}   `json:"dependencyResults,omitempty"`
	CollaborationResults   map[string]interface{}   `json:"collaborationResults,omitempty"`
	ContinuationCheckpoint map[string]interface{}   `json:"continuationCheckpoint,omitempty"`
	PendingInterventions   []AgentRunIntervention   `json:"pendingInterventions,omitempty"`
}

func MarshalHostedTurnModelInput(request HostedTurnRequest) ([]byte, error) {
	return json.Marshal(HostedTurnModelInput{
		Goal: request.Goal, InputContext: request.InputContext, SystemInstructions: request.SystemInstructions,
		EligibleAgents:    request.EligibleAgents,
		RunbookOperations: request.RunbookOperations,
		SkillPrompts:      request.SkillPrompts, Actions: request.Actions, Budget: request.Budget,
		DependencyResults: request.DependencyResults, CollaborationResults: request.CollaborationResults,
		ContinuationCheckpoint: request.ContinuationCheckpoint,
		PendingInterventions:   request.PendingInterventions,
	})
}

func EstimateHostedTurnInputTokens(request HostedTurnRequest) (int64, error) {
	estimateRequest := request
	if request.Budget != nil {
		budget := *request.Budget
		budget.TurnReservation = BudgetUsage{}
		estimateRequest.Budget = &budget
	}
	input, err := MarshalHostedTurnModelInput(estimateRequest)
	if err != nil {
		return 0, err
	}
	return HostedTurnProtocolInputReserveTokens + HostedTurnBudgetEnvelopeReserveTokens + estimateHostedJSONTokens(input), nil
}

func EstimateEvidenceGroundingReviewInputTokens(request EvidenceGroundingRequest) (int64, error) {
	estimate := request
	estimate.MaxOutputTokens = 0
	input, err := MarshalEvidenceGroundingModelInput(estimate)
	if err != nil {
		return 0, err
	}
	return HostedTurnProtocolInputReserveTokens + HostedTurnBudgetEnvelopeReserveTokens + estimateHostedJSONTokens(input), nil
}

// estimateHostedJSONTokens intentionally remains provider-neutral. Two UTF-8
// bytes per token is conservative for the textual goals, schemas, prompts, and
// structured checkpoints admitted by the hosted contract, while avoiding the
// false one-byte-per-token exhaustion that made the canonical 16k minimum
// unusable with built-in management actions. The fixed protocol reserve above
// absorbs host instructions and ordinary tokenizer variance.
func estimateHostedJSONTokens(input []byte) int64 {
	return (int64(len(input)) + 1) / 2
}

// MarshalEvidenceGroundingModelInput strips host-only credential selection
// before the immutable review envelope is presented to a model.
func MarshalEvidenceGroundingModelInput(request EvidenceGroundingRequest) ([]byte, error) {
	request.ModelCredential = nil
	return json.Marshal(request)
}

func (r *HostedTurnRunner) PlanTurnBudget(_ context.Context, input TurnExecutionContext) (BudgetUsage, error) {
	if r == nil || input.Run == nil || input.Turn == nil || input.Run.Budget == nil {
		return BudgetUsage{}, nil
	}
	request, err := r.buildRequest(input)
	if err != nil {
		return BudgetUsage{}, err
	}
	estimatedInput := int64(0)
	snapshot, err := evidenceSnapshotForGrounding(input.Run.Context)
	if err != nil {
		return BudgetUsage{}, err
	}
	applyEvidenceGroundingDraftInstruction(&request, snapshot)
	groundingState, err := parseEvidenceGroundingState(input.Run.Checkpoint)
	if err != nil {
		return BudgetUsage{}, err
	}
	if groundingState != nil && groundingState.Status == evidenceGroundingPendingReview {
		if snapshot == nil || groundingState.SnapshotID != snapshot.ID {
			return BudgetUsage{}, errors.New("evidence grounding checkpoint does not match the immutable Run snapshot")
		}
		estimatedInput, err = EstimateEvidenceGroundingReviewInputTokens(buildEvidenceGroundingRequest(input, snapshot, groundingState, 0))
	} else {
		estimatedInput, err = EstimateHostedTurnInputTokens(request)
	}
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
	if groundingState != nil && groundingState.Status == evidenceGroundingPendingReview && reservation.OutputTokens > EvidenceGroundingReviewOutputLimit {
		reservation.OutputTokens = EvidenceGroundingReviewOutputLimit
	}
	if reservation.OutputTokens > 0 && reservation.OutputTokens < HostedTurnMinimumOutputTokens {
		return BudgetUsage{}, errors.New("hosted output reservation is below the portable minimum")
	}
	return reservation, nil
}
