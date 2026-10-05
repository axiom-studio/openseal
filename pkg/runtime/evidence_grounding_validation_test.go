package runtime

import (
	"encoding/json"
	"math"
	"testing"
)

func TestValidateEvidenceGroundingReviewMatchesCanonicalCheckWithoutMutation(t *testing.T) {
	request := EvidenceGroundingRequest{
		InvocationID: "review-one", Snapshot: EvidenceSnapshot{ID: "snapshot-one"}, ClaimsDigest: "claims-one",
		Claims:      []EvidenceClaim{{ID: "claim-one", Statement: "A supported fact", EvidenceRefs: []string{"observation-one"}}},
		DraftOutput: map[string]interface{}{"report": "Caller-owned draft"},
	}
	valid := EvidenceGroundingReview{
		APIVersion: EvidenceGroundingAPIVersion, InvocationID: request.InvocationID,
		SnapshotID: request.Snapshot.ID, ClaimsDigest: request.ClaimsDigest,
		ReviewerProvider: " reviewer-provider ", ReviewerModel: " reviewer-model ",
		Accepted: true, CoverageComplete: true,
		Findings: []EvidenceGroundingFinding{{ClaimID: " claim-one ", Status: EvidenceGroundingSupported, Summary: " Supported "}},
	}
	for _, tc := range []struct {
		name   string
		change func(*EvidenceGroundingReview)
		valid  bool
	}{
		{"accepted_normalization", func(*EvidenceGroundingReview) {}, true},
		{"valid_declined_verdict", func(r *EvidenceGroundingReview) {
			r.Accepted = false
			r.Findings[0].Status = EvidenceGroundingUncertain
		}, true},
		{"envelope", func(r *EvidenceGroundingReview) { r.SnapshotID = "another-snapshot" }, false},
		{"unknown_claim", func(r *EvidenceGroundingReview) { r.Findings[0].ClaimID = "another-claim" }, false},
		{"foreign_evidence", func(r *EvidenceGroundingReview) { r.Findings[0].EvidenceRefs = []string{"another-observation"} }, false},
		{"duplicate_claim", func(r *EvidenceGroundingReview) { r.Findings = append(r.Findings, r.Findings[0]) }, false},
		{"contradictory_verdict", func(r *EvidenceGroundingReview) { r.Findings[0].Status = EvidenceGroundingUnsupported }, false},
		{"invalid_usage", func(r *EvidenceGroundingReview) { r.Usage.InputTokens = -1 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := cloneEvidenceGroundingReview(&valid)
			tc.change(review)
			beforeReview, _ := json.Marshal(review)
			beforeRequest, _ := json.Marshal(request)
			canonical := cloneEvidenceGroundingReview(review)
			want := validateEvidenceGroundingReview(canonical, request)
			got := ValidateEvidenceGroundingReview(review, request)
			if (got == nil) != tc.valid || (got == nil) != (want == nil) || (got != nil && got.Error() != want.Error()) {
				t.Fatalf("public checker changed canonical verdict: got=%v want=%v", got, want)
			}
			afterReview, _ := json.Marshal(review)
			afterRequest, _ := json.Marshal(request)
			if string(beforeReview) != string(afterReview) || string(beforeRequest) != string(afterRequest) {
				t.Fatal("public checker mutated caller-owned review or evidence ledger")
			}
		})
	}
	if ValidateEvidenceGroundingReview(nil, request) == nil {
		t.Fatal("nil review was accepted")
	}
	// Copy typed fields directly: JSON cloning would erase an invalid NaN usage
	// value and could change the canonical validation result.
	review := valid
	review.Usage.Cost = math.NaN()
	if ValidateEvidenceGroundingReview(&review, request) == nil || !math.IsNaN(review.Usage.Cost) {
		t.Fatal("invalid typed usage was erased or caller-owned usage changed")
	}
}
