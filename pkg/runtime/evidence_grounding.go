package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	EvidenceGroundingAPIVersion        = "openseal.evidence-grounding/v1"
	evidenceGroundingCheckpointKey     = "_opensealEvidenceGrounding"
	evidenceGroundingPendingReview     = "pending_review"
	evidenceGroundingRepairRequired    = "repair_required"
	EvidenceGroundingReviewOutputLimit = int64(8192)
	evidenceGroundingDraftInstruction  = "For evidence-backed completion, put the complete human-readable report in runOutput.report and cite every EvidenceClaim using its exact observation IDs in that report. outputSummary and the claim ledger do not replace the report."
)

func applyEvidenceGroundingDraftInstruction(request *HostedTurnRequest, snapshot *EvidenceSnapshot) {
	if request == nil || snapshot == nil || containsString(request.SystemInstructions, evidenceGroundingDraftInstruction) {
		return
	}
	request.SystemInstructions = append(request.SystemInstructions, evidenceGroundingDraftInstruction)
}

type EvidenceClaim struct {
	ID           string   `json:"id"`
	Statement    string   `json:"statement"`
	EvidenceRefs []string `json:"evidenceRefs"`
}

type EvidenceGroundingFindingStatus string

const (
	EvidenceGroundingSupported   EvidenceGroundingFindingStatus = "supported"
	EvidenceGroundingUnsupported EvidenceGroundingFindingStatus = "unsupported"
	EvidenceGroundingUncertain   EvidenceGroundingFindingStatus = "uncertain"
)

// EvidenceGroundingFinding is concise, operator-visible review evidence. It
// records a verdict without persisting private reviewer reasoning.
type EvidenceGroundingFinding struct {
	ClaimID      string                         `json:"claimId,omitempty"`
	Status       EvidenceGroundingFindingStatus `json:"status"`
	Summary      string                         `json:"summary"`
	EvidenceRefs []string                       `json:"evidenceRefs,omitempty"`
}

type EvidenceGroundingReview struct {
	APIVersion       string                     `json:"apiVersion"`
	InvocationID     string                     `json:"invocationId"`
	SnapshotID       string                     `json:"snapshotId"`
	ClaimsDigest     string                     `json:"claimsDigest"`
	ReviewerProvider string                     `json:"reviewerProvider"`
	ReviewerModel    string                     `json:"reviewerModel"`
	Accepted         bool                       `json:"accepted"`
	CoverageComplete bool                       `json:"coverageComplete"`
	Findings         []EvidenceGroundingFinding `json:"findings"`
	Usage            TurnUsage                  `json:"usage,omitempty"`
}

type EvidenceGroundingRequest struct {
	APIVersion      string                 `json:"apiVersion"`
	InvocationID    string                 `json:"invocationId"`
	Scope           Scope                  `json:"scope"`
	RunID           string                 `json:"runId"`
	TurnID          string                 `json:"turnId"`
	AgentID         string                 `json:"agentId"`
	Goal            string                 `json:"goal"`
	Snapshot        EvidenceSnapshot       `json:"snapshot"`
	Claims          []EvidenceClaim        `json:"claims"`
	ClaimsDigest    string                 `json:"claimsDigest"`
	DraftSummary    string                 `json:"draftSummary"`
	DraftOutput     map[string]interface{} `json:"draftOutput"`
	MaxOutputTokens int64                  `json:"maxOutputTokens,omitempty"`
}

type EvidenceGroundingReviewer interface {
	ReviewEvidenceGrounding(context.Context, EvidenceGroundingRequest) (*EvidenceGroundingReview, error)
}

type evidenceGroundingState struct {
	APIVersion   string                     `json:"apiVersion"`
	Status       string                     `json:"status"`
	SnapshotID   string                     `json:"snapshotId"`
	ClaimsDigest string                     `json:"claimsDigest"`
	Claims       []EvidenceClaim            `json:"claims"`
	DraftSummary string                     `json:"draftSummary"`
	DraftOutput  map[string]interface{}     `json:"draftOutput"`
	Findings     []EvidenceGroundingFinding `json:"findings,omitempty"`
}

func evidenceSnapshotForGrounding(contextValues map[string]interface{}) (*EvidenceSnapshot, error) {
	if contextValues == nil || contextValues[EvidenceSnapshotContextKey] == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(contextValues[EvidenceSnapshotContextKey])
	if err != nil {
		return nil, fmt.Errorf("encode evidence snapshot: %w", err)
	}
	var snapshot EvidenceSnapshot
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode evidence snapshot: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("evidence snapshot contains multiple values")
	}
	if snapshot.APIVersion != evidenceSnapshotAPIVersion || strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.InitiativeID) == "" {
		return nil, errors.New("evidence snapshot has an invalid envelope")
	}
	if snapshot.SelectedCount != len(snapshot.Observations) || snapshot.ObservationLimit < len(snapshot.Observations) || snapshot.ObservationLimit < 1 || snapshot.SummaryRuneLimit < 1 || snapshot.TotalSummaryRuneLimit < 1 {
		return nil, errors.New("evidence snapshot has invalid bounds")
	}
	seen := make(map[string]struct{}, len(snapshot.Observations))
	for _, observation := range snapshot.Observations {
		id := strings.TrimSpace(observation.ID)
		if id == "" || strings.TrimSpace(observation.SourceURI) == "" || observation.ObservedAt.IsZero() || strings.TrimSpace(observation.ContentDigest) == "" || strings.TrimSpace(observation.Summary) == "" {
			return nil, errors.New("evidence snapshot contains an incomplete observation")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, errors.New("evidence snapshot contains a duplicate observation")
		}
		seen[id] = struct{}{}
	}
	identity := snapshot
	identity.ID = ""
	identityBytes, err := json.Marshal(identity)
	if err != nil || hashBytes(identityBytes) != snapshot.ID {
		return nil, errors.New("evidence snapshot identity does not match its contents")
	}
	return &snapshot, nil
}

func normalizeEvidenceClaims(claims []EvidenceClaim, snapshot *EvidenceSnapshot) ([]EvidenceClaim, string, []EvidenceGroundingFinding) {
	allowed := make(map[string]struct{}, len(snapshot.Observations))
	for _, observation := range snapshot.Observations {
		allowed[observation.ID] = struct{}{}
	}
	normalized := make([]EvidenceClaim, 0, len(claims))
	seenClaims := make(map[string]struct{}, len(claims))
	findings := make([]EvidenceGroundingFinding, 0)
	for _, claim := range claims {
		claim.ID = strings.TrimSpace(claim.ID)
		claim.Statement = strings.TrimSpace(claim.Statement)
		if claim.ID == "" || claim.Statement == "" || len(claim.EvidenceRefs) == 0 {
			findings = append(findings, EvidenceGroundingFinding{ClaimID: claim.ID, Status: EvidenceGroundingUnsupported, Summary: "Every material claim requires an identity, statement, and evidence reference."})
			continue
		}
		if _, duplicate := seenClaims[claim.ID]; duplicate {
			findings = append(findings, EvidenceGroundingFinding{ClaimID: claim.ID, Status: EvidenceGroundingUnsupported, Summary: "Claim identities must be unique."})
			continue
		}
		seenClaims[claim.ID] = struct{}{}
		refs := make([]string, 0, len(claim.EvidenceRefs))
		seenRefs := make(map[string]struct{}, len(claim.EvidenceRefs))
		for _, reference := range claim.EvidenceRefs {
			reference = strings.TrimSpace(reference)
			if _, ok := allowed[reference]; !ok {
				findings = append(findings, EvidenceGroundingFinding{ClaimID: claim.ID, Status: EvidenceGroundingUnsupported, Summary: "Claim cites evidence outside the immutable snapshot.", EvidenceRefs: []string{reference}})
				continue
			}
			if _, duplicate := seenRefs[reference]; duplicate {
				continue
			}
			seenRefs[reference] = struct{}{}
			refs = append(refs, reference)
		}
		claim.EvidenceRefs = refs
		normalized = append(normalized, claim)
	}
	if len(normalized) == 0 {
		findings = append(findings, EvidenceGroundingFinding{Status: EvidenceGroundingUnsupported, Summary: "Evidence-backed completion requires a claim ledger covering the draft."})
	}
	encoded, _ := json.Marshal(normalized)
	return normalized, hashBytes(encoded), findings
}

func stageEvidenceGrounding(response *HostedTurnResponse, snapshot *EvidenceSnapshot) {
	claims, digest, findings := normalizeEvidenceClaims(response.EvidenceClaims, snapshot)
	findings = append(findings, validateEvidenceGroundingDraft(response.RunOutput, claims)...)
	status := evidenceGroundingPendingReview
	if len(findings) > 0 {
		status = evidenceGroundingRepairRequired
	}
	checkpoint := cloneMap(response.ContinuationCheckpoint)
	if checkpoint == nil {
		checkpoint = map[string]interface{}{}
	}
	state := evidenceGroundingState{
		APIVersion: EvidenceGroundingAPIVersion, Status: status, SnapshotID: snapshot.ID,
		ClaimsDigest: digest, Claims: claims, DraftSummary: strings.TrimSpace(response.OutputSummary),
		DraftOutput: cloneMap(response.RunOutput), Findings: findings,
	}
	checkpoint[evidenceGroundingCheckpointKey] = evidenceGroundingStateMap(state)
	response.ContinuationCheckpoint = checkpoint
	response.EvidenceClaims = claims
	response.EvidenceGrounding = &EvidenceGroundingReview{
		APIVersion: EvidenceGroundingAPIVersion, InvocationID: response.InvocationID,
		SnapshotID: snapshot.ID, ClaimsDigest: digest, Accepted: false, CoverageComplete: false, Findings: findings,
	}
	response.NextRunStatus = AgentRunStatusRunning
	response.WakeCondition = nil
	response.RunOutput = nil
	if status == evidenceGroundingPendingReview {
		response.OutputSummary = "Draft preserved for a bounded evidence-grounding review."
	} else {
		response.OutputSummary = "Draft requires repair before evidence-grounding review."
	}
}

func validateEvidenceGroundingDraft(output map[string]interface{}, claims []EvidenceClaim) []EvidenceGroundingFinding {
	report, ok := output["report"].(string)
	report = strings.TrimSpace(report)
	if !ok || report == "" {
		return []EvidenceGroundingFinding{{
			Status:  EvidenceGroundingUnsupported,
			Summary: "Evidence-backed completion requires the complete human-readable report in runOutput.report; outputSummary and the claim ledger are not the deliverable.",
		}}
	}
	findings := make([]EvidenceGroundingFinding, 0)
	for _, claim := range claims {
		missing := make([]string, 0)
		for _, reference := range claim.EvidenceRefs {
			if !strings.Contains(report, reference) {
				missing = append(missing, reference)
			}
		}
		if len(missing) > 0 {
			findings = append(findings, EvidenceGroundingFinding{
				ClaimID: claim.ID, Status: EvidenceGroundingUnsupported, EvidenceRefs: missing,
				Summary: "The human-readable report must cite every exact observation ID used by this claim.",
			})
		}
	}
	return findings
}

func evidenceGroundingStateMap(state evidenceGroundingState) map[string]interface{} {
	encoded, _ := json.Marshal(state)
	var projected map[string]interface{}
	_ = json.Unmarshal(encoded, &projected)
	return projected
}

func parseEvidenceGroundingState(checkpoint map[string]interface{}) (*evidenceGroundingState, error) {
	if checkpoint == nil || checkpoint[evidenceGroundingCheckpointKey] == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(checkpoint[evidenceGroundingCheckpointKey])
	if err != nil {
		return nil, err
	}
	var state evidenceGroundingState
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode evidence grounding checkpoint: %w", err)
	}
	if state.APIVersion != EvidenceGroundingAPIVersion || (state.Status != evidenceGroundingPendingReview && state.Status != evidenceGroundingRepairRequired) || strings.TrimSpace(state.SnapshotID) == "" || strings.TrimSpace(state.ClaimsDigest) == "" {
		return nil, errors.New("evidence grounding checkpoint has an invalid envelope")
	}
	claims, _ := json.Marshal(state.Claims)
	if hashBytes(claims) != state.ClaimsDigest {
		return nil, errors.New("evidence grounding checkpoint claim digest does not match its contents")
	}
	if err := ValidateCredentialFreeContext(state.DraftOutput); err != nil {
		return nil, fmt.Errorf("evidence grounding draft: %w", err)
	}
	return &state, nil
}

func buildEvidenceGroundingRequest(input TurnExecutionContext, snapshot *EvidenceSnapshot, state *evidenceGroundingState, maxOutput int64) EvidenceGroundingRequest {
	return EvidenceGroundingRequest{
		APIVersion: EvidenceGroundingAPIVersion, InvocationID: input.Turn.ID,
		Scope: input.Run.Scope, RunID: input.Run.ID, TurnID: input.Turn.ID, AgentID: input.Run.AssignedAgentID, Goal: input.Run.Goal,
		Snapshot: *snapshot, Claims: append([]EvidenceClaim(nil), state.Claims...), ClaimsDigest: state.ClaimsDigest,
		DraftSummary: state.DraftSummary, DraftOutput: cloneMap(state.DraftOutput), MaxOutputTokens: maxOutput,
	}
}

func validateEvidenceGroundingReview(review *EvidenceGroundingReview, request EvidenceGroundingRequest) error {
	if review == nil || review.APIVersion != EvidenceGroundingAPIVersion || review.InvocationID != request.InvocationID || review.SnapshotID != request.Snapshot.ID || review.ClaimsDigest != request.ClaimsDigest {
		return errors.New("evidence reviewer returned a mismatched response envelope")
	}
	var err error
	review.ReviewerProvider, review.ReviewerModel, err = normalizeModelIdentity(review.ReviewerProvider, review.ReviewerModel)
	if err != nil || review.ReviewerProvider == "" {
		return errors.New("evidence reviewer must report its provider and model")
	}
	if err := review.Usage.Validate(); err != nil {
		return err
	}
	claims := make(map[string]EvidenceClaim, len(request.Claims))
	for _, claim := range request.Claims {
		claims[claim.ID] = claim
	}
	seen := make(map[string]struct{}, len(review.Findings))
	allSupported := true
	for index := range review.Findings {
		finding := &review.Findings[index]
		finding.ClaimID = strings.TrimSpace(finding.ClaimID)
		finding.Summary = strings.TrimSpace(finding.Summary)
		if finding.ClaimID == "" || finding.Summary == "" {
			return errors.New("evidence review findings require a claim and summary")
		}
		claim, ok := claims[finding.ClaimID]
		if !ok {
			return errors.New("evidence review cited an unknown claim")
		}
		if _, duplicate := seen[finding.ClaimID]; duplicate {
			return errors.New("evidence review returned duplicate claim findings")
		}
		seen[finding.ClaimID] = struct{}{}
		if finding.Status != EvidenceGroundingSupported && finding.Status != EvidenceGroundingUnsupported && finding.Status != EvidenceGroundingUncertain {
			return errors.New("evidence review returned an invalid finding status")
		}
		if len(finding.EvidenceRefs) == 0 {
			finding.EvidenceRefs = append([]string(nil), claim.EvidenceRefs...)
		}
		allowed := make(map[string]struct{}, len(claim.EvidenceRefs))
		for _, reference := range claim.EvidenceRefs {
			allowed[reference] = struct{}{}
		}
		for _, reference := range finding.EvidenceRefs {
			if _, ok := allowed[reference]; !ok {
				return errors.New("evidence review cited evidence outside the claim ledger")
			}
		}
		if finding.Status != EvidenceGroundingSupported {
			allSupported = false
		}
	}
	if len(seen) != len(claims) {
		allSupported = false
	}
	if review.Accepted != (review.CoverageComplete && allSupported) {
		return errors.New("evidence review acceptance contradicts its findings or coverage")
	}
	return nil
}

func evidenceGroundingResultMap(review *EvidenceGroundingReview) map[string]interface{} {
	encoded, _ := json.Marshal(review)
	var result map[string]interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func evidenceClaimsResult(claims []EvidenceClaim) []interface{} {
	encoded, _ := json.Marshal(claims)
	var result []interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func evidenceSnapshotID(snapshot *EvidenceSnapshot) string {
	if snapshot == nil {
		return ""
	}
	return snapshot.ID
}

func cloneEvidenceGroundingReview(review *EvidenceGroundingReview) *EvidenceGroundingReview {
	if review == nil {
		return nil
	}
	encoded, _ := json.Marshal(review)
	var cloned EvidenceGroundingReview
	_ = json.Unmarshal(encoded, &cloned)
	return &cloned
}

func preserveKernelEvidenceGrounding(existing, proposed map[string]interface{}) map[string]interface{} {
	if existing == nil || existing[evidenceGroundingCheckpointKey] == nil {
		return proposed
	}
	if proposed == nil {
		proposed = map[string]interface{}{}
	}
	proposed[evidenceGroundingCheckpointKey] = cloneMap(map[string]interface{}{
		evidenceGroundingCheckpointKey: existing[evidenceGroundingCheckpointKey],
	})[evidenceGroundingCheckpointKey]
	return proposed
}

func (r *HostedTurnRunner) runEvidenceGroundingReview(ctx context.Context, input TurnExecutionContext, snapshot *EvidenceSnapshot, state *evidenceGroundingState) (*TurnOutcome, error) {
	reviewer, ok := r.host.(EvidenceGroundingReviewer)
	if !ok {
		return nil, errors.New("evidence-backed completion requires a configured grounding reviewer")
	}
	budget, err := projectHostedRunBudget(input.Run, input.Turn.ID)
	if err != nil {
		return nil, err
	}
	maxOutput := EvidenceGroundingReviewOutputLimit
	if budget != nil && budget.TurnReservation.OutputTokens > 0 && budget.TurnReservation.OutputTokens < maxOutput {
		maxOutput = budget.TurnReservation.OutputTokens
	}
	request := buildEvidenceGroundingRequest(input, snapshot, state, maxOutput)
	review, err := reviewer.ReviewEvidenceGrounding(ctx, request)
	if err != nil {
		return nil, retryableTurnHostError{cause: err}
	}
	if err := validateEvidenceGroundingReview(review, request); err != nil {
		return nil, err
	}
	if budget != nil {
		reserved := budget.TurnReservation
		if reserved.InputTokens > 0 && int64(review.Usage.InputTokens) > reserved.InputTokens {
			return nil, fmt.Errorf("evidence reviewer reported %d input tokens beyond the reserved %d", review.Usage.InputTokens, reserved.InputTokens)
		}
		if reserved.OutputTokens > 0 && int64(review.Usage.OutputTokens) > reserved.OutputTokens {
			return nil, fmt.Errorf("evidence reviewer reported %d output tokens beyond the reserved %d", review.Usage.OutputTokens, reserved.OutputTokens)
		}
	}
	checkpoint := cloneMap(input.Run.Checkpoint)
	if review.Accepted {
		delete(checkpoint, evidenceGroundingCheckpointKey)
		output := cloneMap(state.DraftOutput)
		if output == nil {
			output = map[string]interface{}{}
		}
		output["evidenceClaims"] = evidenceClaimsResult(state.Claims)
		output["evidenceGrounding"] = evidenceGroundingResultMap(review)
		references := make([]string, 0, len(snapshot.Observations))
		for _, observation := range snapshot.Observations {
			references = append(references, observation.ID)
		}
		return &TurnOutcome{
			ModelProvider: review.ReviewerProvider, Model: review.ReviewerModel,
			Decisions:     []TurnDecision{{Summary: "Accepted the report after bounded semantic evidence review.", EvidenceRefs: references}},
			OutputSummary: state.DraftSummary, Usage: review.Usage, ContinuationCheckpoint: checkpoint,
			NextRunStatus: AgentRunStatusCompleted, RunOutput: output,
			EvidenceClaims: append([]EvidenceClaim(nil), state.Claims...), EvidenceGrounding: cloneEvidenceGroundingReview(review),
		}, nil
	}
	state.Status = evidenceGroundingRepairRequired
	state.Findings = append([]EvidenceGroundingFinding(nil), review.Findings...)
	checkpoint[evidenceGroundingCheckpointKey] = evidenceGroundingStateMap(*state)
	return &TurnOutcome{
		ModelProvider: review.ReviewerProvider, Model: review.ReviewerModel,
		Decisions:     []TurnDecision{{Summary: "Rejected the draft because one or more claims were unsupported, uncertain, or not covered."}},
		OutputSummary: "Evidence grounding review requested a bounded repair.", Usage: review.Usage,
		ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusRunning,
		EvidenceClaims: append([]EvidenceClaim(nil), state.Claims...), EvidenceGrounding: cloneEvidenceGroundingReview(review),
	}, nil
}
