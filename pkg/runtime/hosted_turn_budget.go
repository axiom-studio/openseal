package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
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
	// HostedTurnMaximumProviderAttempts is the portable upper bound for one
	// hosted Turn invocation, including a single deterministic schema repair.
	// The kernel reserves the complete envelope before dispatch so a repair can
	// never consume unreserved lifetime budget.
	HostedTurnMaximumProviderAttempts int64 = capability.HostedMaximumProviderAttempts
	// HostedTurnRepairInputReserveTokens covers the bounded validation message
	// appended to the second provider request. The rejected model response is
	// not replayed into the repair prompt.
	HostedTurnRepairInputReserveTokens int64 = capability.HostedRepairInputReserveTokens
	HostedTurnMinimumOutputTokens      int64 = 64
	// Hosted child work must be large enough to carry at least one complete
	// provider-neutral hosted protocol exchange with room for bounded recovery.
	// These are protocol floors, not domain-specific recommendations; a model
	// may allocate more when the parent has capacity and the work requires it.
	HostedTurnMinimumChildAttempts     int64 = 3
	HostedTurnMinimumChildTurns        int64 = 2
	HostedTurnMinimumChildInputTokens  int64 = 16384
	HostedTurnMinimumChildOutputTokens int64 = capability.HostedMinimumChildOutputTokens
	HostedTurnMinimumChildTotalTokens  int64 = HostedTurnMinimumChildInputTokens + HostedTurnMinimumChildOutputTokens
	HostedTurnMinimumChildDurationMS   int64 = 180000
	HostedTurnMinimumChildActions      int64 = 1
	// HostedTurnMediaReserveTokens is a conservative provider-neutral reserve
	// for one bounded browser screenshot. Providers report actual usage, which
	// is still settled against the Run budget after the Turn.
	HostedTurnMediaReserveTokens int64 = 4096
	// HostedTurnHistoricalActionResultBytes keeps concise earlier findings in
	// model context while projecting large durable outputs by canonical ActionCall
	// reference. The full evidence remains in the ActionCall store and the most
	// recent result remains available through continuationCheckpoint.lastAction.
	HostedTurnHistoricalActionResultBytes = 2 << 10
	// HostedTurnPressuredActionResultBytes is the same bound for a turn
	// rebuilt after its host reported context pressure.
	HostedTurnPressuredActionResultBytes = 512
)

// HostedTurnModelInput is the credential-free data envelope presented to the
// model. Keeping it portable lets the kernel reserve the same immutable input
// that an enterprise host dispatches.
type HostedTurnModelInput struct {
	Images                    []HostedImageReference           `json:"images,omitempty"`
	Goal                      string                           `json:"goal"`
	InputContext              map[string]interface{}           `json:"inputContext,omitempty"`
	ConversationTasks         *HostedConversationTaskContext   `json:"conversationTasks,omitempty"`
	SystemInstructions        []string                         `json:"systemInstructions,omitempty"`
	EligibleAgents            []HostedAgentTarget              `json:"eligibleAgents,omitempty"`
	WorkspaceOperations       []workspace.Operation            `json:"workspaceOperations,omitempty"`
	RunbookOperations         []HostedRunbookOperation         `json:"runbookOperations,omitempty"`
	SkillPrompts              []HostedSkillPrompt              `json:"skillPrompts,omitempty"`
	Actions                   []capability.ModelAction         `json:"actions,omitempty"`
	ActionInvocationContracts []HostedActionInvocationContract `json:"actionInvocationContracts,omitempty"`
	Budget                    *HostedRunBudget                 `json:"budget,omitempty"`
	DependencyResults         map[string]interface{}           `json:"dependencyResults,omitempty"`
	CollaborationResults      map[string]interface{}           `json:"collaborationResults,omitempty"`
	ContinuationCheckpoint    map[string]interface{}           `json:"continuationCheckpoint,omitempty"`
	PendingInterventions      []AgentRunIntervention           `json:"pendingInterventions,omitempty"`
}

// HostedImageReference identifies each image in the separate provider media
// channel, in the same order, without copying binary data into textual input.
type HostedImageReference struct {
	MediaType             string `json:"mediaType"`
	SourceActionCallID    string `json:"sourceActionCallId,omitempty"`
	SourceArtifactID      string `json:"sourceArtifactId,omitempty"`
	SourceArtifactVersion int64  `json:"sourceArtifactVersion,omitempty"`
	SourceMessageID       string `json:"sourceMessageId,omitempty"`
}

// HostedActionInvocationContract makes the complete model-authored Turn
// envelope explicit for one authorized action. Action inputSchema describes
// only the referenced capability arguments; it cannot describe sibling Turn
// fields such as externalOperation, which models otherwise tend to overlook.
// Keeping this projection in OpenSeal prevents every host from inventing a
// subtly different calling convention.
type HostedActionInvocationContract struct {
	Capability          string                              `json:"capability"`
	ModelAuthoredFields []string                            `json:"modelAuthoredFields"`
	HostOwnedFields     []string                            `json:"hostOwnedFields"`
	ExternalOperation   HostedExternalOperationInstructions `json:"externalOperation"`
	ApprovalReview      string                              `json:"approvalReview"`
	CompletionEvidence  string                              `json:"completionEvidence,omitempty"`
}

type HostedExternalOperationInstructions struct {
	Policy         capability.ExternalOperationPolicy `json:"policy"`
	RequiredFields []string                           `json:"requiredFields,omitempty"`
	Instruction    string                             `json:"instruction"`
	Example        *ExternalOperationIdentity         `json:"example,omitempty"`
}

func ProjectHostedActionInvocationContracts(actions []capability.ModelAction) []HostedActionInvocationContract {
	contracts := make([]HostedActionInvocationContract, 0, len(actions))
	for _, action := range actions {
		fields := []string{"type", "capability", "summary", "idempotencyKey", "inputRef"}
		policy := effectiveExternalOperationPolicy(action.SideEffect, action.ExternalOperationPolicy)
		external := HostedExternalOperationInstructions{Policy: policy}
		switch policy {
		case capability.ExternalOperationRequired:
			fields = append(fields, "externalOperation")
			external.RequiredFields = []string{"resource", "operation"}
			external.Instruction = "Required on proposedAction. resource is the stable HTTP(S) target URL, a single email address (bare or mailto: without headers), or opaque resource ID; operation is the stable semantic effect. Never use a DOM reference, session ID, Run ID, timestamp, credential, or model wording."
			external.Example = &ExternalOperationIdentity{Resource: "https://service.example/resources/42", Operation: "resource:update"}
		case capability.ExternalOperationForbidden:
			external.Instruction = "Forbidden. Omit externalOperation from proposedAction because this action does not commit a durable external business operation."
		default:
			external.Instruction = "Optional. Include externalOperation only when this action commits an externally observable business operation."
		}
		contract := HostedActionInvocationContract{
			Capability:          action.Name,
			ModelAuthoredFields: fields,
			HostOwnedFields:     []string{"bindingId", "bindingRevision", "preparedRuntime"},
			ExternalOperation:   external,
		}
		if policy == capability.ExternalOperationRequired {
			contract.CompletionEvidence = "After the action succeeds, any completion claim about its external effect must cite action-call:<durable actionCallId> in completionEvidenceRefs."
		}
		if action.SideEffect == capability.SideEffectExternal {
			contract.ModelAuthoredFields = append(contract.ModelAuthoredFields, "reviewContext")
			contract.ApprovalReview = "Required. Explain what the reviewer is authorizing in plain language. Include the exact externally visible content when content will be published or sent, the stable target and audience, the purpose, and any material consequences. Include only facts known from durable observations or prior successful actions. Never include authentication material, tokens, passwords, cookies, or hidden reasoning."
		} else {
			contract.ApprovalReview = "Optional. Include reviewContext when an operator would need semantic facts beyond the execution arguments to judge the action. Never include authentication material or hidden reasoning."
		}
		contracts = append(contracts, contract)
	}
	return contracts
}

// effectiveExternalOperationPolicy preserves the documented empty-policy
// behavior only for genuinely external actions. Session-local and preparatory
// actions cannot own a durable cross-Run external-operation identity, so an
// omitted policy is forbidden for them and must be presented that way to the
// model as well as enforced by the kernel.
func effectiveExternalOperationPolicy(sideEffect capability.SideEffect, policy capability.ExternalOperationPolicy) capability.ExternalOperationPolicy {
	if policy != "" {
		return policy
	}
	if sideEffect == capability.SideEffectExternal {
		return capability.ExternalOperationOptional
	}
	return capability.ExternalOperationForbidden
}

func MarshalHostedTurnModelInput(request HostedTurnRequest) ([]byte, error) {
	if err := request.ConversationTasks.Validate(); err != nil {
		return nil, err
	}
	images := make([]HostedImageReference, 0, len(request.ModelMedia))
	for _, media := range request.ModelMedia {
		images = append(images, HostedImageReference{MediaType: media.MediaType, SourceActionCallID: media.SourceActionCallID, SourceArtifactID: media.SourceArtifactID, SourceArtifactVersion: media.SourceArtifactVersion, SourceMessageID: media.SourceMessageID})
	}
	checkpoint := hostedTurnModelCheckpoint(request.ContinuationCheckpoint)
	if request.ContextPressure != nil {
		shortenHostedTurnActionHistory(checkpoint)
	}
	return json.Marshal(HostedTurnModelInput{
		Images: images,
		Goal:   request.Goal, InputContext: request.InputContext, SystemInstructions: request.SystemInstructions,
		ConversationTasks:         cloneHostedConversationTaskContext(request.ConversationTasks),
		EligibleAgents:            request.EligibleAgents,
		WorkspaceOperations:       request.WorkspaceOperations,
		RunbookOperations:         request.RunbookOperations,
		SkillPrompts:              request.SkillPrompts,
		Actions:                   request.Actions,
		ActionInvocationContracts: ProjectHostedActionInvocationContracts(request.Actions),
		Budget:                    request.Budget,
		DependencyResults:         request.DependencyResults, CollaborationResults: request.CollaborationResults,
		ContinuationCheckpoint: checkpoint,
		PendingInterventions:   request.PendingInterventions,
	})
}

// pressuredActionHistorySavings is how many model input bytes shortening
// earlier action results would save for a turn on checkpoint.
func pressuredActionHistorySavings(checkpoint map[string]interface{}) int {
	view := hostedTurnModelCheckpoint(checkpoint)
	before, err := json.Marshal(view)
	if err != nil {
		return 0
	}
	shortenHostedTurnActionHistory(view)
	after, err := json.Marshal(view)
	if err != nil {
		return 0
	}
	return len(before) - len(after)
}

// shortenHostedTurnActionHistory compacts every earlier action result in a
// model view beyond the pressured bound. The current result stays whole.
func shortenHostedTurnActionHistory(checkpoint map[string]interface{}) {
	history, _ := checkpoint[actionHistoryCheckpointKey].([]interface{})
	for _, value := range history {
		entry, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		result, exists := entry["result"]
		if !exists {
			continue
		}
		if object, _ := result.(map[string]interface{}); object["currentResultRef"] != nil {
			continue
		}
		if encoded, err := json.Marshal(result); err == nil && len(encoded) <= HostedTurnPressuredActionResultBytes {
			continue
		}
		entry["result"] = compactHostedTurnActionResult(result, strings.TrimSpace(fmt.Sprint(entry["actionCallId"])), HostedTurnPressuredActionResultBytes)
	}
}

func hostedTurnTextCheckpoint(checkpoint map[string]interface{}) map[string]interface{} {
	// The model projection is allowed to compact and redact, while the durable
	// checkpoint is immutable evidence. A shallow map copy would alias nested
	// action history and media values back into stored Run state.
	result := deepCloneCheckpointMap(checkpoint)
	compactHostedTurnActionHistory(result)
	last, ok := result["lastAction"].(map[string]interface{})
	if !ok {
		return result
	}
	media, ok := last["modelMedia"].(map[string]interface{})
	if !ok {
		return result
	}
	last["modelMedia"] = map[string]interface{}{
		"attached":  true,
		"mediaType": strings.TrimSpace(fmt.Sprint(media["mediaType"])),
		"detail":    strings.TrimSpace(fmt.Sprint(media["detail"])),
	}
	return result
}

// Limits for action arguments shown in the model view of a checkpoint.
const (
	// HostedTurnElidedArgumentStringBytes is the longest argument string kept
	// verbatim in model-visible action history; longer strings (file contents,
	// documents) become a size and digest the model can still compare.
	HostedTurnElidedArgumentStringBytes = 1 << 10
	// HostedTurnFailedActionArgumentBytes keeps a failed last action's
	// arguments whole up to this encoded size, so the model can correct the
	// exact input that was rejected.
	HostedTurnFailedActionArgumentBytes = 16 << 10
)

// hostedTurnModelCheckpoint is the checkpoint exactly as a hosted model sees
// it: the text projection plus elided large action arguments. It always works
// on a deep clone, so durable Run state and any projection stored in it (such
// as approval recovery) are never altered. Action input pointers are resolved
// against the durable checkpoint, never against this view.
func hostedTurnModelCheckpoint(checkpoint map[string]interface{}) map[string]interface{} {
	result := hostedTurnTextCheckpoint(checkpoint)
	elideHostedTurnActionArguments(result)
	return result
}

// elideHostedTurnActionArguments replaces long strings in historical and last
// action arguments with {elided, bytes, sha256}. Paths and other short fields
// stay, so a write[] or files[] entry keeps its path and loses only content.
// Model-authored actionInputs and the proposal recovery marker are untouched,
// and a failed last action keeps its arguments whole when they are small
// enough to correct.
func elideHostedTurnActionArguments(checkpoint map[string]interface{}) {
	if checkpoint == nil {
		return
	}
	if history, ok := checkpoint[actionHistoryCheckpointKey].([]interface{}); ok {
		for _, value := range history {
			if entry, ok := value.(map[string]interface{}); ok {
				if arguments, exists := entry["arguments"]; exists {
					entry["arguments"] = elideHostedTurnArgumentValue(arguments)
				}
			}
		}
	}
	last, ok := checkpoint["lastAction"].(map[string]interface{})
	if !ok {
		return
	}
	arguments, exists := last["arguments"]
	if !exists {
		return
	}
	if status, _ := last["status"].(string); ActionCallStatus(status) == ActionCallStatusFailed {
		if encoded, err := json.Marshal(arguments); err == nil && len(encoded) <= HostedTurnFailedActionArgumentBytes {
			return
		}
	}
	last["arguments"] = elideHostedTurnArgumentValue(arguments)
}

func elideHostedTurnArgumentValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case string:
		if len(typed) <= HostedTurnElidedArgumentStringBytes {
			return typed
		}
		digest := sha256.Sum256([]byte(typed))
		return map[string]interface{}{"elided": true, "bytes": len(typed), "sha256": hex.EncodeToString(digest[:])}
	case map[string]interface{}:
		for key, child := range typed {
			typed[key] = elideHostedTurnArgumentValue(child)
		}
		return typed
	case []interface{}:
		for index, child := range typed {
			typed[index] = elideHostedTurnArgumentValue(child)
		}
		return typed
	default:
		return typed
	}
}

func compactHostedTurnActionHistory(checkpoint map[string]interface{}) {
	if checkpoint == nil {
		return
	}
	last, _ := checkpoint["lastAction"].(map[string]interface{})
	lastActionID, _ := last["actionCallId"].(string)
	lastActionID = strings.TrimSpace(lastActionID)
	history, ok := checkpoint[actionHistoryCheckpointKey].([]interface{})
	if !ok {
		return
	}
	for _, value := range history {
		entry, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		// Preserve one exact copy of large current inputs (documents, slides,
		// code) instead of repeating them in both evidence projections. Only
		// deduplicate equal values belonging to the same real ActionCall.
		// This operates on the deep-cloned model view, never durable evidence.
		if id, _ := entry["actionCallId"].(string); lastActionID != "" && id == lastActionID {
			arguments, present := entry["arguments"]
			latestArguments, latestPresent := last["arguments"]
			if present && latestPresent {
				encoded, encodeErr := json.Marshal(arguments)
				latestEncoded, latestErr := json.Marshal(latestArguments)
				const reference = "continuationCheckpoint.lastAction.arguments"
				if encodeErr == nil && latestErr == nil && len(encoded) > len(reference)+16 && bytes.Equal(encoded, latestEncoded) {
					delete(entry, "arguments")
					entry["argumentsRef"] = reference
				}
			}
		}
		result, exists := entry["result"]
		if !exists {
			continue
		}
		actionCallID := strings.TrimSpace(fmt.Sprint(entry["actionCallId"]))
		encoded, err := json.Marshal(result)
		if lastActionID != "" && actionCallID == lastActionID {
			entry["result"] = map[string]interface{}{
				"compacted": true, "evidenceRef": "action-call:" + actionCallID,
				"currentResultRef": "continuationCheckpoint.lastAction.result",
			}
			continue
		}
		if err != nil || len(encoded) > HostedTurnHistoricalActionResultBytes {
			entry["result"] = compactHostedTurnActionResult(result, actionCallID, HostedTurnHistoricalActionResultBytes)
		}
	}
}

func compactHostedTurnActionResult(result interface{}, actionCallID string, textBytes int) map[string]interface{} {
	compacted := map[string]interface{}{
		"compacted": true, "evidenceRef": "action-call:" + actionCallID,
	}
	object, ok := result.(map[string]interface{})
	if !ok {
		return compacted
	}
	for _, key := range []string{"url", "title", "status", "outcome", "requiresHuman", "generation", "sessionId"} {
		if value, exists := object[key]; exists {
			compacted[key] = deepCloneCheckpointValue(value)
		}
	}
	if value, ok := object["text"].(string); ok && strings.TrimSpace(value) != "" {
		compacted["text"] = boundedHostedTurnEvidenceText(value, textBytes)
	}
	if value, exists := object["challenges"]; exists {
		if encoded, err := json.Marshal(value); err == nil && len(encoded) <= 512 {
			compacted["challenges"] = deepCloneCheckpointValue(value)
		}
	}
	return compacted
}

func boundedHostedTurnEvidenceText(value string, maximumBytes int) string {
	value = strings.ToValidUTF8(value, "")
	if maximumBytes <= 0 || len(value) <= maximumBytes {
		return value
	}
	end := maximumBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return strings.TrimSpace(value[:end]) + "…"
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
	return HostedTurnProtocolInputReserveTokens + HostedTurnBudgetEnvelopeReserveTokens + estimateHostedJSONTokens(input) + int64(len(request.ModelMedia))*HostedTurnMediaReserveTokens, nil
}

// EstimateHostedTurnMaximumInputTokens returns the worst-case input charged by
// the bounded hosted protocol. A host may make one initial provider request and
// one schema-repair request; provider usage is reported cumulatively for the
// durable Turn, so both requests must be reserved atomically.
func EstimateHostedTurnMaximumInputTokens(request HostedTurnRequest) (int64, error) {
	perAttempt, err := EstimateHostedTurnInputTokens(request)
	if err != nil {
		return 0, err
	}
	return perAttempt*HostedTurnMaximumProviderAttempts + HostedTurnRepairInputReserveTokens, nil
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
		estimatedInput, err = EstimateHostedTurnMaximumInputTokens(request)
	}
	if err != nil {
		return BudgetUsage{}, fmt.Errorf("estimate hosted Turn input: %w", err)
	}
	policy := request.Budget.Policy
	remaining := request.Budget.Remaining
	reservation := BudgetUsage{}
	if policy.MaxInputTokens > 0 {
		if remaining.MaxInputTokens < estimatedInput {
			return BudgetUsage{}, &BudgetAdmissionError{Admission: BudgetAdmission{Reason: BudgetAdmissionHostedInput, Dimension: "input_tokens", Required: estimatedInput, Remaining: remaining.MaxInputTokens, Reservation: BudgetUsage{InputTokens: estimatedInput}}}
		}
		reservation.InputTokens = estimatedInput
	}
	if policy.MaxTotalTokens > 0 {
		if remaining.MaxTotalTokens < estimatedInput+HostedTurnMinimumOutputTokens {
			return BudgetUsage{}, &BudgetAdmissionError{Admission: BudgetAdmission{Reason: BudgetAdmissionHostedInput, Dimension: "total_tokens", Required: estimatedInput + HostedTurnMinimumOutputTokens, Remaining: remaining.MaxTotalTokens, Reservation: BudgetUsage{InputTokens: estimatedInput, OutputTokens: HostedTurnMinimumOutputTokens}}}
		}
		reservation.InputTokens = estimatedInput
		reservation.OutputTokens = remaining.MaxTotalTokens - estimatedInput
	}
	if policy.MaxOutputTokens > 0 {
		if remaining.MaxOutputTokens < HostedTurnMinimumOutputTokens {
			return BudgetUsage{}, &BudgetAdmissionError{Admission: BudgetAdmission{Reason: BudgetAdmissionHostedInput, Dimension: "output_tokens", Required: HostedTurnMinimumOutputTokens, Remaining: remaining.MaxOutputTokens, Reservation: BudgetUsage{OutputTokens: HostedTurnMinimumOutputTokens}}}
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
