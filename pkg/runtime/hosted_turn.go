package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

var (
	ErrTurnHostUnavailable   = errors.New("turn host is temporarily unavailable")
	ErrTurnHostConfiguration = errors.New("turn host configuration is invalid")
)

// TurnHostFailure is a safe, stable failure disposition returned by a hosted
// turn boundary. Retryable failures participate in bounded backoff; terminal
// failures end the Run immediately so operator action is not misrepresented as
// an unavailable worker. Message must never contain provider credentials or a
// raw provider response.
type TurnHostFailure struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *TurnHostFailure) Error() string {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return "hosted agent turn failed"
	}
	return strings.TrimSpace(e.Message)
}

func (e *TurnHostFailure) Is(target error) bool {
	return e != nil && e.Retryable && target == ErrTurnHostUnavailable
}

func NewTurnHostFailure(code, message string, retryable bool) error {
	return &TurnHostFailure{Code: strings.TrimSpace(code), Message: strings.TrimSpace(message), Retryable: retryable}
}

type retryableTurnHostError struct{ cause error }

func (e retryableTurnHostError) Error() string        { return ErrTurnHostUnavailable.Error() }
func (e retryableTurnHostError) Unwrap() error        { return e.cause }
func (e retryableTurnHostError) Is(target error) bool { return target == ErrTurnHostUnavailable }

const HostedTurnAPIVersion = "openseal.hosted-turn/v12"

const maximumHostedTurnMediaBytes = 1 << 20

// HostedTurnMedia is ephemeral model-visible binary context produced by a
// governed action. It is carried separately from the textual model envelope so
// base64 never pollutes prompts, checkpoints returned by the model, or token
// estimation. The Turn host decides how to encode it for its provider.
type HostedTurnMedia struct {
	MediaType          string `json:"mediaType"`
	DataBase64         string `json:"dataBase64"`
	Detail             string `json:"detail,omitempty"`
	SourceActionCallID string `json:"sourceActionCallId"`
}

// HostedAgentTarget is one active, same-scope Agent deployment eligible for
// bounded delegation. ID is the only durable identity; DisplayName and Purpose
// are model-facing discovery labels and are never persisted as substitutes.
type HostedAgentTarget struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Purpose     string `json:"purpose,omitempty"`
}

// HostedRunbookOperation is one immutable deterministic operation the current
// cognitive Agent may invoke. It contains no graph internals or credentials;
// the kernel resolves the exact active definition again at materialization.
type HostedRunbookOperation struct {
	Entrypoint   string                 `json:"entrypoint"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	InputSchema  map[string]interface{} `json:"inputSchema"`
	OutputSchema map[string]interface{} `json:"outputSchema,omitempty"`
}

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
	// MinimumChild is the portable hosted-protocol floor for one delegated or
	// forked child. A positive value applies only when the corresponding parent
	// dimension is bounded; clients must compare it with Remaining before
	// proposing child work.
	MinimumChild BudgetPolicy `json:"minimumChild,omitempty"`
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
	EligibleAgents         []HostedAgentTarget      `json:"eligibleAgents,omitempty"`
	RunbookOperations      []HostedRunbookOperation `json:"runbookOperations,omitempty"`
	SkillPrompts           []HostedSkillPrompt      `json:"skillPrompts,omitempty"`
	Actions                []capability.ModelAction `json:"actions,omitempty"`
	Budget                 *HostedRunBudget         `json:"budget,omitempty"`
	DependencyResults      map[string]interface{}   `json:"dependencyResults,omitempty"`
	CollaborationResults   map[string]interface{}   `json:"collaborationResults,omitempty"`
	ContinuationCheckpoint map[string]interface{}   `json:"continuationCheckpoint,omitempty"`
	PendingInterventions   []AgentRunIntervention   `json:"pendingInterventions,omitempty"`
	ModelMedia             []HostedTurnMedia        `json:"modelMedia,omitempty"`
	// ModelCredential is an opaque, host-resolved binding. It crosses only the
	// trusted TurnHost transport boundary and is deliberately excluded from
	// HostedTurnModelInput, durable checkpoints, and model-visible context.
	ModelCredential *capability.CredentialReference `json:"modelCredential,omitempty"`
	ModelProvider   string                          `json:"modelProvider,omitempty"`
	Model           string                          `json:"model,omitempty"`
}

type HostedTurnResponse struct {
	APIVersion             string                   `json:"apiVersion"`
	InvocationID           string                   `json:"invocationId"`
	ModelProvider          string                   `json:"modelProvider"`
	Model                  string                   `json:"model"`
	SkillSelections        []HostedSkillSelection   `json:"skillSelections,omitempty"`
	Decisions              []TurnDecision           `json:"decisions,omitempty"`
	ProposedAction         *TurnAction              `json:"proposedAction,omitempty"`
	ProposedFork           *TurnForkProposal        `json:"proposedFork,omitempty"`
	ProposedDelegation     *TurnDelegationProposal  `json:"proposedDelegation,omitempty"`
	ProposedRunbook        *TurnRunbookProposal     `json:"proposedRunbook,omitempty"`
	OutputSummary          string                   `json:"outputSummary"`
	Usage                  TurnUsage                `json:"usage,omitempty"`
	ContinuationCheckpoint map[string]interface{}   `json:"continuationCheckpoint,omitempty"`
	NextRunStatus          AgentRunStatus           `json:"nextRunStatus"`
	WakeCondition          *WakeCondition           `json:"wakeCondition,omitempty"`
	RunOutput              map[string]interface{}   `json:"runOutput,omitempty"`
	RunError               string                   `json:"runError,omitempty"`
	CompletionEvidenceRefs []string                 `json:"completionEvidenceRefs,omitempty"`
	EvidenceClaims         []EvidenceClaim          `json:"evidenceClaims,omitempty"`
	EvidenceGrounding      *EvidenceGroundingReview `json:"evidenceGrounding,omitempty"`
}

// ValidateHostedSkillSelections verifies that a host returned exactly one
// operator-visible disposition for every authorized prompt Skill and no other
// Skill. Hosts can call this before returning a response so mechanically
// repairable model output is corrected inside the bounded invocation rather
// than failing later in the durable Run lifecycle.
func ValidateHostedSkillSelections(prompts []HostedSkillPrompt, selections []HostedSkillSelection) error {
	allowed := make(map[string]struct{}, len(prompts))
	for _, prompt := range prompts {
		allowed[prompt.Reference] = struct{}{}
	}
	if len(selections) != len(allowed) {
		return errors.New("turn host must disposition every offered Skill")
	}
	selected := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		reference := strings.TrimSpace(selection.SkillRef)
		if _, ok := allowed[reference]; !ok {
			return fmt.Errorf("turn host dispositioned unauthorized Skill reference %q", reference)
		}
		if _, duplicate := selected[reference]; duplicate {
			return errors.New("turn host dispositioned a Skill more than once")
		}
		if strings.TrimSpace(selection.Summary) == "" ||
			(selection.Disposition != HostedSkillApplied && selection.Disposition != HostedSkillNotApplied) {
			return errors.New("turn host returned an invalid Skill disposition")
		}
		selected[reference] = struct{}{}
	}
	return nil
}

type TurnHost interface {
	ExecuteHostedTurn(context.Context, HostedTurnRequest) (*HostedTurnResponse, error)
}

type HostedTurnRunnerConfig struct {
	AgentID            string
	ActionDeploymentID string
	DefinitionID       string
	DefinitionVersion  string
	SystemInstructions []string
	EligibleAgents     []HostedAgentTarget
	RunbookOperations  []HostedRunbookOperation
	SkillPrompts       []HostedSkillPrompt
	Actions            []capability.ModelAction
	ModelCredential    *capability.CredentialReference
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
	if config.ModelCredential != nil && (strings.TrimSpace(config.ModelCredential.Kind) == "" || strings.TrimSpace(config.ModelCredential.ID) == "") {
		return nil, errors.New("model credential reference requires kind and id")
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
	seenAgents := make(map[string]bool, len(config.EligibleAgents))
	for _, target := range config.EligibleAgents {
		if strings.TrimSpace(target.ID) == "" || strings.TrimSpace(target.DisplayName) == "" {
			return nil, errors.New("eligible hosted Agents require canonical id and display name")
		}
		if seenAgents[target.ID] {
			return nil, fmt.Errorf("eligible hosted Agent ID %q is duplicated", target.ID)
		}
		seenAgents[target.ID] = true
	}
	seenRunbooks := make(map[string]bool, len(config.RunbookOperations))
	for _, operation := range config.RunbookOperations {
		if !validOpaqueIdentifier(operation.Entrypoint, 128) || strings.TrimSpace(operation.Name) == "" ||
			strings.TrimSpace(operation.Description) == "" || operation.InputSchema["type"] != "object" {
			return nil, errors.New("hosted runbook operations require entrypoint, name, description, and an object input schema")
		}
		if seenRunbooks[operation.Entrypoint] {
			return nil, fmt.Errorf("hosted runbook entrypoint %q is duplicated", operation.Entrypoint)
		}
		seenRunbooks[operation.Entrypoint] = true
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
		if errors.Is(err, ErrTurnHostConfiguration) {
			return nil, err
		}
		var failure *TurnHostFailure
		if errors.As(err, &failure) {
			return nil, err
		}
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
	if response.NextRunStatus == AgentRunStatusWaitingForAgent && response.WakeCondition != nil &&
		strings.TrimSpace(response.WakeCondition.Reference) == strings.TrimSpace(r.config.AgentID) {
		response.NextRunStatus = AgentRunStatusRunning
		response.WakeCondition = nil
		response.Decisions = append(response.Decisions, TurnDecision{
			Summary: "Continued the Run because an Agent cannot wait on itself.",
		})
	}
	if err := response.Usage.Validate(); err != nil {
		return nil, err
	}
	if request.Budget != nil {
		reserved := request.Budget.TurnReservation
		if reserved.InputTokens > 0 && int64(response.Usage.InputTokens) > reserved.InputTokens {
			actualInput := int64(response.Usage.InputTokens)
			remaining := request.Budget.Remaining
			actualTotal := actualInput + int64(response.Usage.OutputTokens)
			if (remaining.MaxInputTokens > 0 && actualInput > remaining.MaxInputTokens) ||
				(remaining.MaxTotalTokens > 0 && actualTotal > remaining.MaxTotalTokens) {
				return nil, fmt.Errorf("turn host reported %d input tokens beyond the remaining Run budget", response.Usage.InputTokens)
			}
		}
		if reserved.OutputTokens > 0 && int64(response.Usage.OutputTokens) > reserved.OutputTokens {
			return nil, fmt.Errorf("turn host reported %d output tokens beyond the reserved %d", response.Usage.OutputTokens, reserved.OutputTokens)
		}
	}
	allowed := make(map[string]capability.ModelAction, len(request.Actions))
	for _, action := range request.Actions {
		allowed[action.Name] = action
	}
	if proposed := response.ProposedAction; proposed != nil {
		action, ok := allowed[proposed.Capability]
		if !ok {
			return nil, errors.New("turn host proposed an unauthorized capability")
		}
		externalOperationPolicy := effectiveExternalOperationPolicy(action.SideEffect, action.ExternalOperationPolicy)
		if externalOperationPolicy == capability.ExternalOperationRequired && proposed.ExternalOperation == nil {
			return nil, errors.New("action requires an external operation identity")
		}
		if proposed.ExternalOperation != nil {
			if externalOperationPolicy == capability.ExternalOperationForbidden {
				return nil, errors.New("action forbids an external operation identity")
			}
			if action.SideEffect != capability.SideEffectExternal {
				return nil, errors.New("external operation identity is only valid for an external side effect")
			}
			if err := proposed.ExternalOperation.Validate(); err != nil {
				return nil, err
			}
			resource, _ := canonicalExternalOperationResource(proposed.ExternalOperation.Resource)
			proposed.ExternalOperation = &ExternalOperationIdentity{
				Resource: resource, Operation: strings.ToLower(strings.TrimSpace(proposed.ExternalOperation.Operation)),
			}
		}
	}
	response.ContinuationCheckpoint = preserveKernelActionHistory(request.ContinuationCheckpoint, response.ContinuationCheckpoint)
	response.ContinuationCheckpoint = preserveKernelEvidenceGrounding(request.ContinuationCheckpoint, response.ContinuationCheckpoint)
	if response.ProposedAction != nil {
		if reused, reuseErr := r.reuseSucceededAction(*response.ProposedAction, response.ContinuationCheckpoint); reuseErr != nil {
			return nil, reuseErr
		} else if reused != nil {
			changed, _, _, hasProgress := actionProgress(reused["result"])
			response.ProposedAction = nil
			response.ProposedFork = nil
			response.ProposedDelegation = nil
			response.ProposedRunbook = nil
			response.NextRunStatus = AgentRunStatusRunning
			response.WakeCondition = nil
			response.ContinuationCheckpoint["lastAction"] = deepCloneCheckpointMap(reused)
			decisionSummary := "Reused the matching succeeded action instead of executing it again."
			response.OutputSummary = "A matching succeeded action was reused; continue from its durable evidence."
			if hasProgress && !changed {
				decisionSummary = "Blocked a repeated action that previously made no observable progress. Choose a different action, request intervention, or fail truthfully."
				response.OutputSummary = "The repeated action was not executed because the same intent previously left the authoritative observation unchanged."
			}
			response.Decisions = append(response.Decisions, TurnDecision{
				Summary:      decisionSummary,
				EvidenceRefs: []string{"action:" + fmt.Sprint(reused["actionCallId"])},
			})
		}
	}
	proposalCount := 0
	if response.ProposedAction != nil {
		proposalCount++
	}
	if response.ProposedFork != nil {
		proposalCount++
		for index := range response.ProposedFork.Branches {
			response.ProposedFork.Branches[index].AssignedAgentID, err = resolveHostedAgentTarget(
				response.ProposedFork.Branches[index].AssignedAgentID, request.EligibleAgents,
			)
			if err != nil {
				return nil, fmt.Errorf("invalid hosted fork branch %s target: %w", response.ProposedFork.Branches[index].ID, err)
			}
		}
		if err := response.ProposedFork.Validate(); err != nil {
			return nil, fmt.Errorf("invalid hosted fork proposal: %w", err)
		}
		allocations := make([]*BudgetPolicy, 0, len(response.ProposedFork.Branches))
		for _, branch := range response.ProposedFork.Branches {
			if err := validateHostedChildBudgetFloor(branch.Budget, request.Budget); err != nil {
				return nil, fmt.Errorf("invalid hosted fork branch %s budget: %w", branch.ID, err)
			}
			allocations = append(allocations, branch.Budget)
		}
		if err := validateHostedChildBudgetCapacity(allocations, request.Budget); err != nil {
			return nil, fmt.Errorf("invalid hosted fork budget: %w", err)
		}
	}
	if response.ProposedDelegation != nil {
		proposalCount++
		response.ProposedDelegation.AssignedAgentID, err = resolveHostedAgentTarget(
			response.ProposedDelegation.AssignedAgentID, request.EligibleAgents,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid hosted delegation target: %w", err)
		}
		if err := response.ProposedDelegation.Validate(); err != nil {
			return nil, fmt.Errorf("invalid hosted delegation proposal: %w", err)
		}
		if err := validateHostedChildBudgetFloor(response.ProposedDelegation.Budget, request.Budget); err != nil {
			return nil, fmt.Errorf("invalid hosted delegation budget: %w", err)
		}
		if err := validateHostedChildBudgetCapacity([]*BudgetPolicy{response.ProposedDelegation.Budget}, request.Budget); err != nil {
			return nil, fmt.Errorf("invalid hosted delegation budget: %w", err)
		}
	}
	if response.ProposedRunbook != nil {
		proposalCount++
		if err := response.ProposedRunbook.Validate(); err != nil {
			return nil, fmt.Errorf("invalid hosted runbook proposal: %w", err)
		}
		var selected *HostedRunbookOperation
		for _, operation := range request.RunbookOperations {
			if operation.Entrypoint == response.ProposedRunbook.Entrypoint {
				copy := operation
				selected = &copy
				break
			}
		}
		if selected == nil {
			return nil, errors.New("turn host proposed an unauthorized runbook entrypoint")
		}
		if err := runbook.ValidateInterfaceInput(selected.InputSchema, response.ProposedRunbook.Arguments); err != nil {
			return nil, fmt.Errorf("invalid hosted runbook arguments: %w", err)
		}
		if err := validateHostedChildBudgetFloor(response.ProposedRunbook.Budget, request.Budget); err != nil {
			return nil, fmt.Errorf("invalid hosted runbook budget: %w", err)
		}
		if err := validateHostedChildBudgetCapacity([]*BudgetPolicy{response.ProposedRunbook.Budget}, request.Budget); err != nil {
			return nil, fmt.Errorf("invalid hosted runbook budget: %w", err)
		}
	}
	if proposalCount > 1 {
		return nil, errors.New("a bounded hosted Turn can propose only one action, fork, delegation, or runbook")
	}
	if proposalCount == 1 && response.NextRunStatus != AgentRunStatusRunning {
		return nil, errors.New("a hosted Turn proposal must remain running until the kernel materializes it")
	}
	if err := ValidateHostedSkillSelections(request.SkillPrompts, response.SkillSelections); err != nil {
		return nil, err
	}
	if err := ValidateHostedTurnCompletion(request, response); err != nil {
		return nil, err
	}
	allowedSkillRefs := make(map[string]struct{}, len(request.SkillPrompts))
	for _, prompt := range request.SkillPrompts {
		allowedSkillRefs[prompt.Reference] = struct{}{}
	}
	selectionDecisions := make([]TurnDecision, 0, len(response.SkillSelections))
	for _, selection := range response.SkillSelections {
		selection.SkillRef = strings.TrimSpace(selection.SkillRef)
		selection.Summary = strings.TrimSpace(selection.Summary)
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
	proposedActions := []TurnAction(nil)
	if response.ProposedAction != nil {
		proposedActions = []TurnAction{*response.ProposedAction}
	}
	return &TurnOutcome{
		ModelProvider: response.ModelProvider, Model: response.Model,
		SkillSelections: append([]HostedSkillSelection(nil), response.SkillSelections...),
		Decisions:       append(selectionDecisions, response.Decisions...), ProposedActions: proposedActions,
		ProposedFork: response.ProposedFork, ProposedDelegation: response.ProposedDelegation, ProposedRunbook: response.ProposedRunbook,
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
	actionDeploymentID := strings.TrimSpace(r.config.ActionDeploymentID)
	if actionDeploymentID == "" {
		actionDeploymentID = r.config.AgentID
	}
	call := &ActionCall{
		DeploymentID: actionDeploymentID, BindingID: selected.BindingID, BindingRevision: selected.BindingRevision,
		SkillID: selected.SkillID, SkillVersion: selected.Version, Action: selected.Action, Arguments: arguments,
	}
	// A rejected proposal may require a new authoritative read before it can be
	// corrected. Historical read reuse would leave the rejected proposal as the
	// latest action and make that recovery impossible. Execute the safe
	// prerequisite again; the successful action clears the recovery marker.
	if checkpoint[proposalRecoveryCheckpointKey] != nil &&
		(selected.SideEffect == capability.SideEffectRead || selected.SideEffect == capability.SideEffectNone) {
		return nil, nil
	}
	if entry := matchingNoProgressAction(checkpoint, computeActionProgressIntentDigest(call, selected.SemanticArguments)); entry != nil {
		return entry, nil
	}
	digest := ComputeActionSemanticDigest(call)
	if selected.SideEffect == capability.SideEffectRead || selected.SideEffect == capability.SideEffectNone {
		entries := actionHistoryEntries(checkpoint)
		if len(entries) == 0 {
			return nil, nil
		}
		latest := entries[len(entries)-1]
		status := latest["status"]
		if latest["semanticDigest"] == digest && (status == string(ActionCallStatusSucceeded) || status == ActionCallStatusSucceeded) {
			return latest, nil
		}
		return nil, nil
	}
	return succeededActionHistoryEntry(checkpoint, digest), nil
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
	collaborationResults, err := projectHostedCollaborationResults(input.Run)
	if err != nil {
		return HostedTurnRequest{}, err
	}
	inputContext := cloneMap(input.Run.Context)
	request := HostedTurnRequest{
		APIVersion: HostedTurnAPIVersion, InvocationID: input.Turn.ID,
		Scope: input.Run.Scope, RunID: input.Run.ID, TurnID: input.Turn.ID,
		AgentID: r.config.AgentID, DefinitionID: r.config.DefinitionID, DefinitionVersion: r.config.DefinitionVersion,
		Goal: input.Run.Goal, InputContext: inputContext, SystemInstructions: append([]string(nil), r.config.SystemInstructions...),
		EligibleAgents:         cloneHostedAgentTargets(r.config.EligibleAgents),
		RunbookOperations:      cloneHostedRunbookOperations(r.config.RunbookOperations),
		SkillPrompts:           cloneHostedSkillPrompts(r.config.SkillPrompts),
		Actions:                cloneHostedModelActions(r.config.Actions),
		Budget:                 budget,
		DependencyResults:      dependencyResults,
		CollaborationResults:   collaborationResults,
		ContinuationCheckpoint: cloneMap(input.Run.Checkpoint),
		PendingInterventions:   append([]AgentRunIntervention(nil), input.Run.PendingInterventions...),
		ModelMedia:             hostedTurnMediaFromCheckpoint(input.Run.Checkpoint),
		ModelCredential:        cloneHostedCredentialReference(r.config.ModelCredential),
		ModelProvider:          r.config.ModelProvider, Model: r.config.Model,
	}
	if input.Run.Checkpoint != nil && input.Run.Checkpoint[approvalRecoveryCheckpointKey] != nil {
		request.SystemInstructions = append(request.SystemInstructions,
			"A previously approved exact action failed. Continue from continuationCheckpoint._opensealApprovalRecovery and its approval-owned continuationCheckpoint. The prior approval authorizes only the recorded proposedAction. Reuse its durable context; any materially changed action is a new proposal and must pass policy and approval independently. Do not restart completed setup unless the recorded failure requires replacing that setup.")
	}
	if input.Run.Checkpoint != nil && input.Run.Checkpoint[proposalRecoveryCheckpointKey] != nil {
		request.SystemInstructions = append(request.SystemInstructions,
			"The previous proposed action was rejected before execution. Read continuationCheckpoint._opensealProposalRecovery and the rejected arguments in continuationCheckpoint.actionInputs, then correct the exact validation error. Do not propose the same capability again with the same semantically invalid argument. If an observed value does not satisfy that capability's authorized schema or description, choose the authorized action that matches the observed value's role, complete any prerequisite, obtain fresh authoritative input, and only then retry the original operation. The rejected action was never approved or executed.")
	}
	for _, media := range request.ModelMedia {
		if err := validateHostedTurnMedia(media); err != nil {
			return HostedTurnRequest{}, err
		}
	}
	for index := range request.SkillPrompts {
		request.SkillPrompts[index].Reference = hostedSkillPromptReference(request.SkillPrompts[index])
	}
	if err := applyHostedMinimumChildBudget(&request); err != nil {
		return HostedTurnRequest{}, err
	}
	return request, nil
}

func hostedTurnMediaFromCheckpoint(checkpoint map[string]interface{}) []HostedTurnMedia {
	last, ok := checkpoint["lastAction"].(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := last["modelMedia"].(map[string]interface{})
	if !ok {
		return nil
	}
	return []HostedTurnMedia{{
		MediaType:          strings.TrimSpace(fmt.Sprint(raw["mediaType"])),
		DataBase64:         strings.TrimSpace(fmt.Sprint(raw["contentBase64"])),
		Detail:             strings.TrimSpace(fmt.Sprint(raw["detail"])),
		SourceActionCallID: strings.TrimSpace(fmt.Sprint(last["actionCallId"])),
	}}
}

func validateHostedTurnMedia(media HostedTurnMedia) error {
	if media.MediaType != "image/png" && media.MediaType != "image/jpeg" && media.MediaType != "image/webp" {
		return errors.New("hosted Turn media must be a supported image")
	}
	if media.Detail != "" && media.Detail != "low" && media.Detail != "high" && media.Detail != "auto" {
		return errors.New("hosted Turn media detail must be low, high, or auto")
	}
	if strings.TrimSpace(media.SourceActionCallID) == "" {
		return errors.New("hosted Turn media requires its source ActionCall")
	}
	decoded, err := base64.StdEncoding.DecodeString(media.DataBase64)
	if err != nil || len(decoded) == 0 || len(decoded) > maximumHostedTurnMediaBytes {
		return errors.New("hosted Turn media payload is invalid or exceeds 1 MiB")
	}
	return nil
}

func cloneHostedRunbookOperations(values []HostedRunbookOperation) []HostedRunbookOperation {
	cloned := make([]HostedRunbookOperation, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].InputSchema = cloneMap(value.InputSchema)
		cloned[index].OutputSchema = cloneMap(value.OutputSchema)
	}
	return cloned
}

func resolveHostedAgentTarget(reference string, eligible []HostedAgentTarget) (string, error) {
	reference = strings.TrimSpace(reference)
	for _, target := range eligible {
		if reference == target.ID {
			return target.ID, nil
		}
	}
	matches := make([]string, 0, 1)
	for _, target := range eligible {
		if strings.EqualFold(reference, strings.TrimSpace(target.DisplayName)) {
			matches = append(matches, target.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("Agent name %q is ambiguous; use canonical ID %s", reference, strings.Join(matches, " or "))
	}
	ids := make([]string, 0, len(eligible))
	for _, target := range eligible {
		ids = append(ids, target.ID)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("Agent %q is not eligible; no delegation targets are available", reference)
	}
	return "", fmt.Errorf("Agent %q is not eligible; use canonical ID %s", reference, strings.Join(ids, ", "))
}

func cloneHostedCredentialReference(reference *capability.CredentialReference) *capability.CredentialReference {
	if reference == nil {
		return nil
	}
	cloned := *reference
	return &cloned
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

// projectHostedCollaborationResults exposes only explicitly shared AgentRequest
// results. General Run output remains private to the owning Agent.
func projectHostedCollaborationResults(run *AgentRun) (map[string]interface{}, error) {
	if run == nil || run.Output == nil {
		return nil, nil
	}
	raw, exists := run.Output["collaborationResults"]
	if !exists || raw == nil {
		return nil, nil
	}
	results, ok := raw.(map[string]interface{})
	if !ok {
		return nil, errors.New("project hosted collaboration results: durable collaborationResults output is invalid")
	}
	projected := cloneMap(results)
	if err := ValidateCredentialFreeContext(projected); err != nil {
		return nil, fmt.Errorf("project hosted collaboration results: %w", err)
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
		MinimumChild: hostedMinimumChildBudget(policy),
	}, nil
}

func hostedMinimumChildBudget(policy BudgetPolicy) BudgetPolicy {
	minimum := BudgetPolicy{
		MaxAttempts: HostedTurnMinimumChildAttempts, MaxTurns: HostedTurnMinimumChildTurns,
		MaxInputTokens: HostedTurnMinimumChildInputTokens, MaxOutputTokens: HostedTurnMinimumChildOutputTokens,
		MaxTotalTokens: HostedTurnMinimumChildTotalTokens, MaxCostMicros: 1,
		MaxDurationMS: HostedTurnMinimumChildDurationMS, MaxActions: HostedTurnMinimumChildActions,
	}
	if policy.MaxAttempts == 0 {
		minimum.MaxAttempts = 0
	}
	if policy.MaxTurns == 0 {
		minimum.MaxTurns = 0
	}
	if policy.MaxInputTokens == 0 {
		minimum.MaxInputTokens = 0
	}
	if policy.MaxOutputTokens == 0 {
		minimum.MaxOutputTokens = 0
	}
	if policy.MaxTotalTokens == 0 {
		minimum.MaxTotalTokens = 0
	}
	if policy.MaxCostMicros == 0 {
		minimum.MaxCostMicros = 0
	}
	if policy.MaxDurationMS == 0 {
		minimum.MaxDurationMS = 0
	}
	if policy.MaxActions == 0 {
		minimum.MaxActions = 0
	}
	return minimum
}

func applyHostedMinimumChildBudget(request *HostedTurnRequest) error {
	if request == nil || request.Budget == nil {
		return nil
	}
	minimum := hostedMinimumChildBudget(request.Budget.Policy)
	request.Budget.MinimumChild = minimum
	estimatedInput, err := EstimateHostedTurnMaximumInputTokens(*request)
	if err != nil {
		return fmt.Errorf("estimate minimum hosted child budget: %w", err)
	}
	envelopeInput := estimatedInput + HostedTurnBudgetEnvelopeReserveTokens
	if minimum.MaxInputTokens > 0 && envelopeInput > minimum.MaxInputTokens {
		minimum.MaxInputTokens = envelopeInput
	}
	if minimum.MaxTotalTokens > 0 {
		minimumTotal := envelopeInput + HostedTurnMinimumChildOutputTokens
		if minimumTotal > minimum.MaxTotalTokens {
			minimum.MaxTotalTokens = minimumTotal
		}
	}
	request.Budget.MinimumChild = minimum
	return nil
}

func validateHostedChildBudgetFloor(allocation *BudgetPolicy, budget *HostedRunBudget) error {
	if budget == nil {
		return nil
	}
	actual := BudgetPolicy{}
	if allocation != nil {
		actual = *allocation
	}
	for _, dimension := range []struct {
		name          string
		actual, floor int64
	}{
		{"maxAttempts", actual.MaxAttempts, budget.MinimumChild.MaxAttempts},
		{"maxTurns", actual.MaxTurns, budget.MinimumChild.MaxTurns},
		{"maxInputTokens", actual.MaxInputTokens, budget.MinimumChild.MaxInputTokens},
		{"maxOutputTokens", actual.MaxOutputTokens, budget.MinimumChild.MaxOutputTokens},
		{"maxTotalTokens", actual.MaxTotalTokens, budget.MinimumChild.MaxTotalTokens},
		{"maxCostMicros", actual.MaxCostMicros, budget.MinimumChild.MaxCostMicros},
		{"maxDurationMs", actual.MaxDurationMS, budget.MinimumChild.MaxDurationMS},
		{"maxActions", actual.MaxActions, budget.MinimumChild.MaxActions},
	} {
		if dimension.floor > 0 && dimension.actual < dimension.floor {
			return fmt.Errorf("%s %d is below the hosted minimum %d", dimension.name, dimension.actual, dimension.floor)
		}
	}
	return nil
}

func validateHostedChildBudgetCapacity(allocations []*BudgetPolicy, budget *HostedRunBudget) error {
	if budget == nil {
		return nil
	}
	for _, dimension := range []struct {
		name      string
		bounded   bool
		remaining int64
		value     func(BudgetPolicy) int64
	}{
		{"maxAttempts", budget.Policy.MaxAttempts > 0, budget.Remaining.MaxAttempts, func(policy BudgetPolicy) int64 { return policy.MaxAttempts }},
		{"maxTurns", budget.Policy.MaxTurns > 0, budget.Remaining.MaxTurns, func(policy BudgetPolicy) int64 { return policy.MaxTurns }},
		{"maxInputTokens", budget.Policy.MaxInputTokens > 0, budget.Remaining.MaxInputTokens, func(policy BudgetPolicy) int64 { return policy.MaxInputTokens }},
		{"maxOutputTokens", budget.Policy.MaxOutputTokens > 0, budget.Remaining.MaxOutputTokens, func(policy BudgetPolicy) int64 { return policy.MaxOutputTokens }},
		{"maxTotalTokens", budget.Policy.MaxTotalTokens > 0, budget.Remaining.MaxTotalTokens, func(policy BudgetPolicy) int64 { return policy.MaxTotalTokens }},
		{"maxCostMicros", budget.Policy.MaxCostMicros > 0, budget.Remaining.MaxCostMicros, func(policy BudgetPolicy) int64 { return policy.MaxCostMicros }},
		{"maxDurationMs", budget.Policy.MaxDurationMS > 0, budget.Remaining.MaxDurationMS, func(policy BudgetPolicy) int64 { return policy.MaxDurationMS }},
		{"maxActions", budget.Policy.MaxActions > 0, budget.Remaining.MaxActions, func(policy BudgetPolicy) int64 { return policy.MaxActions }},
	} {
		if !dimension.bounded {
			continue
		}
		total := int64(0)
		for _, allocation := range allocations {
			actual := int64(0)
			if allocation != nil {
				actual = dimension.value(*allocation)
			}
			if actual > dimension.remaining-total {
				return fmt.Errorf("%s allocations exceed hosted remaining capacity %d", dimension.name, dimension.remaining)
			}
			total += actual
		}
	}
	return nil
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

func cloneHostedAgentTargets(values []HostedAgentTarget) []HostedAgentTarget {
	if values == nil {
		return nil
	}
	return append([]HostedAgentTarget(nil), values...)
}
