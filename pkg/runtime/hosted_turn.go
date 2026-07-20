package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

var ErrTurnHostUnavailable = errors.New("turn host is temporarily unavailable")

type retryableTurnHostError struct{ cause error }

func (e retryableTurnHostError) Error() string        { return ErrTurnHostUnavailable.Error() }
func (e retryableTurnHostError) Unwrap() error        { return e.cause }
func (e retryableTurnHostError) Is(target error) bool { return target == ErrTurnHostUnavailable }

const HostedTurnAPIVersion = "openseal.hosted-turn/v7"

// HostedSkillPrompt is an immutable, already-authorized prompt projection. It
// contains no binding configuration or credential value.
type HostedSkillPrompt struct {
	SkillID         string `json:"skillId"`
	Version         string `json:"version"`
	BindingID       string `json:"bindingId,omitempty"`
	BindingRevision int64  `json:"bindingRevision,omitempty"`
	Reference       string `json:"reference"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Instructions    string `json:"instructions"`
}

type HostedSkillDisposition string

const (
	HostedSkillApplied    HostedSkillDisposition = "applied"
	HostedSkillNotApplied HostedSkillDisposition = "not_applied"
)

// HostedSkillSelection is the exhaustive operator-facing disposition of one
// prompt Skill offered to a hosted turn. It records capability choice without
// storing hidden model reasoning.
type HostedSkillSelection struct {
	SkillRef    string                 `json:"skillRef"`
	Disposition HostedSkillDisposition `json:"disposition"`
	Summary     string                 `json:"summary"`
}

// HostedRunBudget is the non-secret, provider-neutral capacity visible to one
// hosted Turn. Policy distinguishes an unbounded zero from an exhausted zero;
// Remaining has already accounted for committed usage, live reservations, and
// durable child allocations so a model can propose valid bounded child work.
type HostedRunBudget struct {
	Policy          BudgetPolicy `json:"policy"`
	CommittedUsage  BudgetUsage  `json:"committedUsage,omitempty"`
	EffectiveUsage  BudgetUsage  `json:"effectiveUsage,omitempty"`
	Allocated       BudgetPolicy `json:"allocated,omitempty"`
	TurnReservation BudgetUsage  `json:"turnReservation,omitempty"`
	Remaining       BudgetPolicy `json:"remaining"`
}

// HostedTurnRequest is the portable execution envelope sent to an Agent host.
// OpenSeal remains authoritative for leases, Turns, actions and state changes;
// the host performs one bounded proposal-only model invocation.
type HostedTurnRequest struct {
	APIVersion             string                   `json:"apiVersion"`
	InvocationID           string                   `json:"invocationId"`
	Scope                  Scope                    `json:"scope"`
	RunID                  string                   `json:"runId"`
	TurnID                 string                   `json:"turnId"`
	AgentID                string                   `json:"agentId"`
	DefinitionID           string                   `json:"definitionId"`
	DefinitionVersion      string                   `json:"definitionVersion"`
	Goal                   string                   `json:"goal"`
	InputContext           map[string]interface{}   `json:"inputContext,omitempty"`
	SystemInstructions     []string                 `json:"systemInstructions,omitempty"`
	SkillPrompts           []HostedSkillPrompt      `json:"skillPrompts,omitempty"`
	Actions                []capability.ModelAction `json:"actions,omitempty"`
	Budget                 *HostedRunBudget         `json:"budget,omitempty"`
	DependencyResults      map[string]interface{}   `json:"dependencyResults,omitempty"`
	ContinuationCheckpoint map[string]interface{}   `json:"continuationCheckpoint,omitempty"`
	PendingInterventions   []AgentRunIntervention   `json:"pendingInterventions,omitempty"`
	ModelProvider          string                   `json:"modelProvider,omitempty"`
	Model                  string                   `json:"model,omitempty"`
}

type HostedTurnResponse struct {
	APIVersion             string                   `json:"apiVersion"`
	InvocationID           string                   `json:"invocationId"`
	ModelProvider          string                   `json:"modelProvider"`
	Model                  string                   `json:"model"`
	SkillSelections        []HostedSkillSelection   `json:"skillSelections,omitempty"`
	Decisions              []TurnDecision           `json:"decisions,omitempty"`
	ProposedActions        []TurnAction             `json:"proposedActions,omitempty"`
	ProposedFork           *TurnForkProposal        `json:"proposedFork,omitempty"`
	ProposedDelegation     *TurnDelegationProposal  `json:"proposedDelegation,omitempty"`
	OutputSummary          string                   `json:"outputSummary"`
	Usage                  TurnUsage                `json:"usage,omitempty"`
	ContinuationCheckpoint map[string]interface{}   `json:"continuationCheckpoint,omitempty"`
	NextRunStatus          AgentRunStatus           `json:"nextRunStatus"`
	WakeCondition          *WakeCondition           `json:"wakeCondition,omitempty"`
	RunOutput              map[string]interface{}   `json:"runOutput,omitempty"`
	RunError               string                   `json:"runError,omitempty"`
	EvidenceClaims         []EvidenceClaim          `json:"evidenceClaims,omitempty"`
	EvidenceGrounding      *EvidenceGroundingReview `json:"evidenceGrounding,omitempty"`
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
	Actions            []capability.ModelAction
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
	seenPromptReferences := make(map[string]bool, len(config.SkillPrompts))
	for _, prompt := range config.SkillPrompts {
		if strings.TrimSpace(prompt.SkillID) == "" || strings.TrimSpace(prompt.Version) == "" || strings.TrimSpace(prompt.Instructions) == "" {
			return nil, errors.New("hosted Skill prompts require identity, version, and instructions")
		}
		if (strings.TrimSpace(prompt.BindingID) == "") != (prompt.BindingRevision == 0) || prompt.BindingRevision < 0 {
			return nil, errors.New("hosted Skill prompts require both binding id and revision when selecting an exact binding")
		}
		reference := hostedSkillPromptReference(prompt)
		if seenPromptReferences[reference] {
			return nil, fmt.Errorf("hosted Skill prompt reference %q is ambiguous", reference)
		}
		seenPromptReferences[reference] = true
	}
	return &HostedTurnRunner{host: host, config: config}, nil
}

func (r *HostedTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || r.host == nil || input.Run == nil || input.Turn == nil {
		return nil, errors.New("hosted turn requires a durable Run and Turn")
	}
	snapshot, err := evidenceSnapshotForGrounding(input.Run.Context)
	if err != nil {
		return nil, err
	}
	groundingState, err := parseEvidenceGroundingState(input.Run.Checkpoint)
	if err != nil {
		return nil, err
	}
	if groundingState != nil && groundingState.SnapshotID != evidenceSnapshotID(snapshot) {
		return nil, errors.New("evidence grounding checkpoint does not match the immutable Run snapshot")
	}
	if groundingState != nil && groundingState.Status == evidenceGroundingPendingReview {
		return r.runEvidenceGroundingReview(ctx, input, snapshot, groundingState)
	}
	request, err := r.buildRequest(input)
	if err != nil {
		return nil, err
	}
	applyEvidenceGroundingDraftInstruction(&request, snapshot)
	response, err := r.host.ExecuteHostedTurn(ctx, request)
	if err != nil {
		return nil, retryableTurnHostError{cause: err}
	}
	if response == nil || response.APIVersion != HostedTurnAPIVersion || response.InvocationID != input.Turn.ID {
		return nil, errors.New("turn host returned a mismatched response envelope")
	}
	response.ModelProvider, response.Model, err = normalizeModelIdentity(response.ModelProvider, response.Model)
	if err != nil {
		return nil, err
	}
	if response.ModelProvider == "" || response.Model == "" {
		return nil, errors.New("turn host must report the actual model provider and model")
	}
	if err := response.Usage.Validate(); err != nil {
		return nil, err
	}
	if request.Budget != nil {
		reserved := request.Budget.TurnReservation
		if reserved.InputTokens > 0 && int64(response.Usage.InputTokens) > reserved.InputTokens {
			return nil, fmt.Errorf("turn host reported %d input tokens beyond the reserved %d", response.Usage.InputTokens, reserved.InputTokens)
		}
		if reserved.OutputTokens > 0 && int64(response.Usage.OutputTokens) > reserved.OutputTokens {
			return nil, fmt.Errorf("turn host reported %d output tokens beyond the reserved %d", response.Usage.OutputTokens, reserved.OutputTokens)
		}
	}
	allowed := make(map[string]struct{}, len(request.Actions))
	for _, action := range request.Actions {
		allowed[action.Name] = struct{}{}
	}
	for _, proposed := range response.ProposedActions {
		if _, ok := allowed[proposed.Capability]; !ok {
			return nil, errors.New("turn host proposed an unauthorized capability")
		}
	}
	response.ContinuationCheckpoint = preserveKernelActionHistory(request.ContinuationCheckpoint, response.ContinuationCheckpoint)
	response.ContinuationCheckpoint = preserveKernelEvidenceGrounding(request.ContinuationCheckpoint, response.ContinuationCheckpoint)
	if len(response.ProposedActions) == 1 {
		if reused, reuseErr := r.reuseSucceededAction(response.ProposedActions[0], response.ContinuationCheckpoint); reuseErr != nil {
			return nil, reuseErr
		} else if reused != nil {
			response.ProposedActions = nil
			response.ProposedFork = nil
			response.ProposedDelegation = nil
			response.NextRunStatus = AgentRunStatusRunning
			response.WakeCondition = nil
			response.ContinuationCheckpoint["lastAction"] = deepCloneCheckpointMap(reused)
			response.Decisions = append(response.Decisions, TurnDecision{
				Summary:      "Reused the matching succeeded action instead of executing it again.",
				EvidenceRefs: []string{"action:" + fmt.Sprint(reused["actionCallId"])},
			})
			response.OutputSummary = "A matching succeeded action was reused; continue from its durable evidence."
		}
	}
	if len(response.ProposedActions) > 1 {
		return nil, errors.New("a bounded hosted Turn can propose at most one action")
	}
	proposalCount := 0
	if len(response.ProposedActions) == 1 {
		proposalCount++
	}
	if response.ProposedFork != nil {
		proposalCount++
		if err := response.ProposedFork.Validate(); err != nil {
			return nil, fmt.Errorf("invalid hosted fork proposal: %w", err)
		}
	}
	if response.ProposedDelegation != nil {
		proposalCount++
		if err := response.ProposedDelegation.Validate(); err != nil {
			return nil, fmt.Errorf("invalid hosted delegation proposal: %w", err)
		}
	}
	if proposalCount > 1 {
		return nil, errors.New("a bounded hosted Turn can propose only one action, fork, or delegation")
	}
	if proposalCount == 1 && response.NextRunStatus != AgentRunStatusRunning {
		return nil, errors.New("a hosted Turn proposal must remain running until the kernel materializes it")
	}
	allowedSkillRefs := make(map[string]struct{}, len(request.SkillPrompts))
	for _, prompt := range request.SkillPrompts {
		allowedSkillRefs[prompt.Reference] = struct{}{}
	}
	if len(response.SkillSelections) != len(allowedSkillRefs) {
		return nil, errors.New("turn host must disposition every offered Skill")
	}
	selectionDecisions := make([]TurnDecision, 0, len(response.SkillSelections))
	selected := make(map[string]struct{}, len(response.SkillSelections))
	for _, selection := range response.SkillSelections {
		selection.SkillRef = strings.TrimSpace(selection.SkillRef)
		selection.Summary = strings.TrimSpace(selection.Summary)
		if _, ok := allowedSkillRefs[selection.SkillRef]; !ok {
			return nil, fmt.Errorf("turn host dispositioned unauthorized Skill reference %q", selection.SkillRef)
		}
		if _, duplicate := selected[selection.SkillRef]; duplicate {
			return nil, errors.New("turn host dispositioned a Skill more than once")
		}
		if selection.Summary == "" || (selection.Disposition != HostedSkillApplied && selection.Disposition != HostedSkillNotApplied) {
			return nil, errors.New("turn host returned an invalid Skill disposition")
		}
		selected[selection.SkillRef] = struct{}{}
		decision := TurnDecision{Summary: selection.Summary}
		if selection.Disposition == HostedSkillApplied {
			decision.EvidenceRefs = []string{selection.SkillRef}
		}
		selectionDecisions = append(selectionDecisions, decision)
	}
	for _, decision := range response.Decisions {
		for _, evidenceRef := range decision.EvidenceRefs {
			evidenceRef = strings.TrimSpace(evidenceRef)
			if !strings.HasPrefix(evidenceRef, "skill:") {
				continue
			}
			if _, ok := allowedSkillRefs[evidenceRef]; !ok {
				return nil, errors.New("turn host cited an unauthorized Skill")
			}
		}
	}
	if snapshot != nil && response.NextRunStatus == AgentRunStatusCompleted {
		stageEvidenceGrounding(response, snapshot)
	}
	return &TurnOutcome{
		ModelProvider: response.ModelProvider, Model: response.Model,
		SkillSelections: append([]HostedSkillSelection(nil), response.SkillSelections...),
		Decisions:       append(selectionDecisions, response.Decisions...), ProposedActions: append([]TurnAction(nil), response.ProposedActions...),
		ProposedFork: response.ProposedFork, ProposedDelegation: response.ProposedDelegation,
		OutputSummary: response.OutputSummary, Usage: response.Usage,
		ContinuationCheckpoint: cloneMap(response.ContinuationCheckpoint), NextRunStatus: response.NextRunStatus,
		WakeCondition: cloneWakeCondition(response.WakeCondition), RunOutput: cloneMap(response.RunOutput), RunError: response.RunError,
		EvidenceClaims: append([]EvidenceClaim(nil), response.EvidenceClaims...), EvidenceGrounding: cloneEvidenceGroundingReview(response.EvidenceGrounding),
	}, nil
}

func (r *HostedTurnRunner) reuseSucceededAction(proposed TurnAction, checkpoint map[string]interface{}) (map[string]interface{}, error) {
	var selected *capability.ModelAction
	for index := range r.config.Actions {
		candidate := &r.config.Actions[index]
		if candidate.Name != proposed.Capability || (proposed.BindingID != "" && (candidate.BindingID != proposed.BindingID || candidate.BindingRevision != proposed.BindingRevision)) {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("requested capability %q is ambiguous without an exact binding", proposed.Capability)
		}
		selected = candidate
	}
	if selected == nil {
		return nil, nil
	}
	arguments, err := resolveTurnActionInput(checkpoint, proposed.InputRef)
	if err != nil {
		return nil, err
	}
	call := &ActionCall{
		DeploymentID: r.config.AgentID, BindingID: selected.BindingID, BindingRevision: selected.BindingRevision,
		SkillID: selected.SkillID, SkillVersion: selected.Version, Action: selected.Action, Arguments: arguments,
	}
	return succeededActionHistoryEntry(checkpoint, ComputeActionSemanticDigest(call)), nil
}

func (r *HostedTurnRunner) buildRequest(input TurnExecutionContext) (HostedTurnRequest, error) {
	budget, err := projectHostedRunBudget(input.Run, input.Turn.ID)
	if err != nil {
		return HostedTurnRequest{}, err
	}
	dependencyResults, err := projectHostedDependencyResults(input.Run)
	if err != nil {
		return HostedTurnRequest{}, err
	}
	request := HostedTurnRequest{
		APIVersion: HostedTurnAPIVersion, InvocationID: input.Turn.ID,
		Scope: input.Run.Scope, RunID: input.Run.ID, TurnID: input.Turn.ID,
		AgentID: r.config.AgentID, DefinitionID: r.config.DefinitionID, DefinitionVersion: r.config.DefinitionVersion,
		Goal: input.Run.Goal, InputContext: cloneMap(input.Run.Context), SystemInstructions: append([]string(nil), r.config.SystemInstructions...),
		SkillPrompts:           cloneHostedSkillPrompts(r.config.SkillPrompts),
		Actions:                cloneHostedModelActions(r.config.Actions),
		Budget:                 budget,
		DependencyResults:      dependencyResults,
		ContinuationCheckpoint: cloneMap(input.Run.Checkpoint),
		PendingInterventions:   append([]AgentRunIntervention(nil), input.Run.PendingInterventions...),
		ModelProvider:          r.config.ModelProvider, Model: r.config.Model,
	}
	for index := range request.SkillPrompts {
		request.SkillPrompts[index].Reference = hostedSkillPromptReference(request.SkillPrompts[index])
	}
	return request, nil
}

func hostedSkillPromptReference(prompt HostedSkillPrompt) string {
	reference := "skill:" + strings.TrimSpace(prompt.SkillID) + "@" + strings.TrimSpace(prompt.Version)
	if strings.TrimSpace(prompt.BindingID) != "" && prompt.BindingRevision > 0 {
		reference += fmt.Sprintf("#binding:%s@%d", strings.TrimSpace(prompt.BindingID), prompt.BindingRevision)
	}
	return reference
}

// projectHostedDependencyResults exposes only the durable fan-in projection,
// not the Run's general output. Child results are the explicit cross-Agent
// return channel and must remain credential-free before becoming model input.
func projectHostedDependencyResults(run *AgentRun) (map[string]interface{}, error) {
	if run == nil || run.Output == nil {
		return nil, nil
	}
	raw, exists := run.Output["dependencyGroups"]
	if !exists || raw == nil {
		return nil, nil
	}
	groups, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("project hosted dependency results: durable dependencyGroups output is invalid")
	}
	projected := cloneMap(groups)
	if err := ValidateCredentialFreeContext(projected); err != nil {
		return nil, fmt.Errorf("project hosted dependency results: %w", err)
	}
	return projected, nil
}

func projectHostedRunBudget(run *AgentRun, currentTurnID string) (*HostedRunBudget, error) {
	if run == nil || run.Budget == nil {
		return nil, nil
	}
	otherReservations := make(map[string]BudgetReservation, len(run.BudgetReservations))
	currentReservation := BudgetUsage{}
	for id, reservation := range run.BudgetReservations {
		if id == currentTurnID {
			currentReservation = reservation.Usage
			continue
		}
		otherReservations[id] = reservation
	}
	effective, err := EffectiveBudgetUsage(run.BudgetUsage, otherReservations)
	if err != nil {
		return nil, fmt.Errorf("project hosted Run budget: %w", err)
	}
	policy := *cloneBudgetPolicy(run.Budget)
	allocated := sumBudgetPolicies(run.BudgetAllocations)
	remaining := BudgetPolicy{
		MaxAttempts:     remainingBudgetDimension(policy.MaxAttempts, effective.Attempts, allocated.MaxAttempts),
		MaxTurns:        remainingBudgetDimension(policy.MaxTurns, effective.Turns, allocated.MaxTurns),
		MaxInputTokens:  remainingBudgetDimension(policy.MaxInputTokens, effective.InputTokens, allocated.MaxInputTokens),
		MaxOutputTokens: remainingBudgetDimension(policy.MaxOutputTokens, effective.OutputTokens, allocated.MaxOutputTokens),
		MaxTotalTokens:  remainingBudgetDimension(policy.MaxTotalTokens, effective.InputTokens+effective.OutputTokens, allocated.MaxTotalTokens),
		MaxCostMicros:   remainingBudgetDimension(policy.MaxCostMicros, effective.CostMicros, allocated.MaxCostMicros),
		MaxDurationMS:   remainingBudgetDimension(policy.MaxDurationMS, effective.DurationMS, allocated.MaxDurationMS),
		MaxActions:      remainingBudgetDimension(policy.MaxActions, effective.Actions, allocated.MaxActions),
	}
	return &HostedRunBudget{
		Policy: policy, CommittedUsage: run.BudgetUsage, EffectiveUsage: effective,
		Allocated: allocated, TurnReservation: currentReservation, Remaining: remaining,
	}, nil
}

func remainingBudgetDimension(limit, used, allocated int64) int64 {
	if limit == 0 {
		return 0
	}
	remaining := limit - used - allocated
	if remaining < 0 {
		return 0
	}
	return remaining
}

func cloneHostedModelActions(values []capability.ModelAction) []capability.ModelAction {
	if values == nil {
		return nil
	}
	result := make([]capability.ModelAction, len(values))
	for index, value := range values {
		result[index] = value
		result[index].InputSchema = cloneMap(value.InputSchema)
		if value.SemanticArguments != nil {
			result[index].SemanticArguments = make(map[string]string, len(value.SemanticArguments))
			for role, argument := range value.SemanticArguments {
				result[index].SemanticArguments[role] = argument
			}
		}
	}
	return result
}

func cloneHostedSkillPrompts(values []HostedSkillPrompt) []HostedSkillPrompt {
	if values == nil {
		return nil
	}
	return append([]HostedSkillPrompt(nil), values...)
}
