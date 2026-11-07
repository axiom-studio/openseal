package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	EvidenceGroundingAPIVersion        = "openseal.evidence-grounding/v1"
	EvidenceGroundingReviewOutputLimit = int64(8192)
)

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
	// ModelCredential is transport-only host configuration and must never be
	// included in the evidence review model input.
	ModelCredential *capability.CredentialReference `json:"modelCredential,omitempty"`
}

type EvidenceGroundingReviewer interface {
	ReviewEvidenceGrounding(context.Context, EvidenceGroundingRequest) (*EvidenceGroundingReview, error)
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
	if snapshot.APIVersion != evidenceSnapshotAPIVersion || strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.ProjectID) == "" {
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

// ValidateEvidenceGroundingReview checks a review against the canonical evidence
// ledger without normalizing or changing either caller-owned argument. It does
// not run a reviewer or grant authority to the review.
func ValidateEvidenceGroundingReview(review *EvidenceGroundingReview, request EvidenceGroundingRequest) error {
	if review == nil {
		return validateEvidenceGroundingReview(nil, request)
	}
	cloned := *review
	cloned.Findings = append([]EvidenceGroundingFinding(nil), review.Findings...)
	for index := range cloned.Findings {
		cloned.Findings[index].EvidenceRefs = append([]string(nil), review.Findings[index].EvidenceRefs...)
	}
	return validateEvidenceGroundingReview(&cloned, request)
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

// MarshalEvidenceGroundingModelInput strips host-only credential selection
// before the immutable review envelope is presented to a model.
func MarshalEvidenceGroundingModelInput(request EvidenceGroundingRequest) ([]byte, error) {
	request.ModelCredential = nil
	return json.Marshal(request)
}
