package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUnsupportedURLRejected(t *testing.T) {
	f := newFinalFixture(t,
		LoopToolCall{ID: "read", Name: "action_web_read", Arguments: json.RawMessage(`{"query":"release notes"}`)},
		finalCall("bad", `{"answer":"See https://invented.test/notes for details."}`),
		finalCall("again", `{"answer":"As I said, https://invented.test/notes."}`),
		finalCall("ok", `{"answer":"The notes are at https://docs.example.test/notes."}`),
	)
	f.governedEntry("read", "web", "read", map[string]interface{}{"url": "https://docs.example.test/notes", "text": "v2 ships"})
	value := requireRejected(t, f, f.settleFinal("bad", ControlAuthority{}), "bad", finalUnsupportedURLCode)
	if value["message"] != "The answer cites https://invented.test/notes, which does not appear in anything you read." {
		t.Fatalf("message = %v", value["message"])
	}
	// The refused final (its arguments and its rejection) is not evidence.
	requireRejected(t, f, f.settleFinal("again", ControlAuthority{}), "again", finalUnsupportedURLCode)
	// Trailing punctuation is trimmed; the read tool's result holds the URL.
	final := requireFinal(t, f.settleFinal("ok", ControlAuthority{}))
	if !strings.Contains(final.Answer, "https://docs.example.test/notes") {
		t.Fatalf("final = %#v", final)
	}
	// A plain-text reply is checked the same way and refused with a note.
	requireRejectionNote(t, f.plain("step-2", 2, ControlAuthority{}, "Read https://invented.test/other"), "step-2", finalUnsupportedURLCode)
	// URLs in the Run goal count as read.
	g := newFinalFixture(t, finalCall("final", `{"answer":"Summarized https://goal.example.test/page."}`))
	g.run.Goal = "Summarize https://goal.example.test/page"
	requireFinal(t, g.settleFinal("final", ControlAuthority{}))
}

func TestExternalClaimNeedsReceipt(t *testing.T) {
	f := newFinalFixture(t,
		LoopToolCall{ID: "send", Name: "action_mail_send", Arguments: json.RawMessage(`{"to":"a@example.test"}`)},
		finalCall("claim", `{"answer":"Done: email sent to Ana."}`),
		finalCall("claim2", `{"answer":"Done: email sent to Ana."}`),
	)
	f.governedEntry("send", "mail", "send", map[string]interface{}{"status": "queued"})
	requireRejected(t, f, f.settleFinal("claim", ControlAuthority{}), "claim", finalUnevidencedClaimCode)
	// The same call with an external operation receipt evidences the claim.
	f.store.mu.Lock()
	receipt := cloneActionCall(f.store.actions["action-send"])
	receipt.ExternalOperationDigest = "sha256:" + strings.Repeat("a", 64)
	f.store.actions[portfolioKey(f.run.Scope, receipt.ID)] = receipt
	f.store.mu.Unlock()
	requireFinal(t, f.settleFinal("claim2", ControlAuthority{}))
	// Without any governed call the claim is refused.
	g := newFinalFixture(t, finalCall("final", `{"answer":"I posted the comment."}`))
	requireRejected(t, g, g.settleFinal("final", ControlAuthority{}), "final", finalUnevidencedClaimCode)
}

func TestApprovalClaimOnceThenFailsRun(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("first", `{"answer":"Your approval request has been submitted."}`),
		finalCall("second", `{"answer":"This is waiting for your approval."}`),
	)
	value := requireRejected(t, f, f.settleFinal("first", ControlAuthority{}), "first", finalApprovalClaimCode)
	if value["message"] != "No approval is pending in this Run; say what actually happened." {
		t.Fatalf("message = %v", value["message"])
	}
	result := f.settleFinal("second", ControlAuthority{})
	if result.Kind != ControlFailed || result.Failure == nil || result.Failure.Code != finalUnverifiedApprovalCode ||
		result.Failure.Kind != RunFailureKindExecution || result.Entry == nil || result.Entry.State != ToolEntryError {
		t.Fatalf("second = %#v", result)
	}
	requireControlError(t, f.stored("second").Tool.Result, ToolResultFailed, finalUnverifiedApprovalCode)
	// The plain-text path counts the same way.
	g := newFinalFixture(t)
	requireRejectionNote(t, g.plain("step-2", 2, ControlAuthority{}, "Pending approval from your admin."), "step-2", finalApprovalClaimCode)
	failed := g.plain("step-3", 3, ControlAuthority{}, "Awaiting approval.")
	if failed.Kind != ControlFailed || failed.Failure == nil || failed.Failure.Code != finalUnverifiedApprovalCode {
		t.Fatalf("plain second = %#v", failed)
	}
	// Saying something was approved is not a pending-approval claim.
	h := newFinalFixture(t, finalCall("final", `{"answer":"The change was approved and merged."}`))
	requireFinal(t, h.settleFinal("final", ControlAuthority{}))
}

// fakeGroundingReviewer accepts every claim unless reject names its id.
type fakeGroundingReviewer struct {
	reject   string
	requests []EvidenceGroundingRequest
}

func (r *fakeGroundingReviewer) ReviewEvidenceGrounding(_ context.Context, request EvidenceGroundingRequest) (*EvidenceGroundingReview, error) {
	r.requests = append(r.requests, request)
	accepted := true
	findings := make([]EvidenceGroundingFinding, 0, len(request.Claims))
	for _, claim := range request.Claims {
		finding := EvidenceGroundingFinding{ClaimID: claim.ID, Status: EvidenceGroundingSupported, Summary: "Supported."}
		if claim.ID == r.reject {
			finding.Status, finding.Summary, accepted = EvidenceGroundingUnsupported, "The observation says nothing about growth.", false
		}
		findings = append(findings, finding)
	}
	return &EvidenceGroundingReview{
		APIVersion: EvidenceGroundingAPIVersion, InvocationID: request.InvocationID,
		SnapshotID: request.Snapshot.ID, ClaimsDigest: request.ClaimsDigest,
		ReviewerProvider: "review-provider", ReviewerModel: "review-model",
		Accepted: accepted, CoverageComplete: true, Findings: findings,
	}, nil
}

func finalChecksSnapshotContext(t *testing.T) map[string]interface{} {
	t.Helper()
	snapshot := &EvidenceSnapshot{
		APIVersion: evidenceSnapshotAPIVersion, ProjectID: "project-one",
		SelectedCount: 2, ObservationLimit: 5, SummaryRuneLimit: 600, TotalSummaryRuneLimit: 3000,
		Observations: []EvidenceSnapshotObservation{
			{ID: "obs-revenue", SourceURI: "https://example.test/revenue", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ContentDigest: "sha256:revenue", Summary: "Revenue grew 12% in Q3."},
			{ID: "obs-churn", SourceURI: "https://example.test/churn", ObservedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), ContentDigest: "sha256:churn", Summary: "Churn stayed flat."},
		},
	}
	identity, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ID = hashBytes(identity)
	projection, err := evidenceSnapshotContext(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]interface{}{EvidenceSnapshotContextKey: projection}
}

func TestEvidenceReviewRejectedIsRepairResult(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("unknown-ref", `{"answer":"x","report":"Revenue grew (obs-made-up).","evidenceClaims":[{"id":"c1","statement":"Revenue grew.","evidenceRefs":["obs-made-up"]}]}`),
		finalCall("uncited", `{"answer":"x","report":"Revenue grew.","evidenceClaims":[{"id":"c1","statement":"Revenue grew.","evidenceRefs":["obs-revenue"]}]}`),
		finalCall("reviewed", `{"answer":"x","report":"Revenue grew (obs-churn).","evidenceClaims":[{"id":"c1","statement":"Revenue grew.","evidenceRefs":["obs-churn"]}]}`),
	)
	f.run.Context = finalChecksSnapshotContext(t)
	reviewer := &fakeGroundingReviewer{reject: "c1"}
	f.exec.Reviewer = reviewer
	evidence := ControlAuthority{FinalMode: FinalModeEvidence}
	value := requireRejected(t, f, f.settleFinal("unknown-ref", evidence), "unknown-ref", finalEvidenceRepairCode)
	if !strings.Contains(value["message"].(string), "outside the immutable snapshot") || len(value["findings"].([]interface{})) == 0 {
		t.Fatalf("unknown ref = %v", value)
	}
	value = requireRejected(t, f, f.settleFinal("uncited", evidence), "uncited", finalEvidenceRepairCode)
	if !strings.Contains(value["message"].(string), "c1: The human-readable report must cite") {
		t.Fatalf("uncited = %v", value)
	}
	if len(reviewer.requests) != 0 {
		t.Fatalf("reviewer called for a draft with findings: %d", len(reviewer.requests))
	}
	value = requireRejected(t, f, f.settleFinal("reviewed", evidence), "reviewed", finalEvidenceRepairCode)
	findings := value["findings"].([]interface{})
	if len(reviewer.requests) != 1 || len(findings) != 1 || findings[0].(map[string]interface{})["status"] != "unsupported" ||
		!strings.Contains(value["message"].(string), "The observation says nothing about growth.") {
		t.Fatalf("reviewed = %v (requests %d)", value, len(reviewer.requests))
	}
	// Nothing is staged in the checkpoint.
	if f.run.Checkpoint[evidenceGroundingCheckpointKey] != nil {
		t.Fatalf("checkpoint = %v", f.run.Checkpoint)
	}
}

func TestEvidenceReviewAcceptedDelivers(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("final", `{"answer":"Q3 summary","report":"Revenue grew 12% (obs-revenue); churn was flat (obs-churn).","evidenceClaims":[
			{"id":" c1 ","statement":"Revenue grew 12%.","evidenceRefs":["obs-revenue"," obs-revenue"]},
			{"id":"c2","statement":"Churn was flat.","evidenceRefs":["obs-churn"]}]}`),
	)
	f.run.Context = finalChecksSnapshotContext(t)
	reviewer := &fakeGroundingReviewer{}
	f.exec.Reviewer = reviewer
	final := requireFinal(t, f.settleFinal("final", ControlAuthority{FinalMode: FinalModeEvidence}))
	if len(reviewer.requests) != 1 {
		t.Fatalf("reviews = %d", len(reviewer.requests))
	}
	request := reviewer.requests[0]
	if request.InvocationID != f.entries["final"].ID || request.TurnID != "step-1" || request.RunID != f.run.ID ||
		request.DraftOutput["report"] != "Revenue grew 12% (obs-revenue); churn was flat (obs-churn)." {
		t.Fatalf("request = %#v", request)
	}
	claims, _ := final.Output[finalOutputEvidenceClaimsKey].([]EvidenceClaim)
	if len(claims) != 2 || claims[0].ID != "c1" || len(claims[0].EvidenceRefs) != 1 {
		t.Fatalf("claims = %#v", final.Output[finalOutputEvidenceClaimsKey])
	}
	grounding, _ := final.Output[finalOutputEvidenceGroundingKey].(map[string]interface{})
	if grounding["accepted"] != true || grounding["reviewerModel"] != "review-model" || grounding["claimsDigest"] != request.ClaimsDigest {
		t.Fatalf("grounding = %#v", grounding)
	}
	if final.Answer != "Revenue grew 12% (obs-revenue); churn was flat (obs-churn)." {
		t.Fatalf("answer = %q", final.Answer)
	}
	// Without a reviewer an evidence final cannot be accepted.
	g := newFinalFixture(t, finalCall("final", `{"answer":"x","report":"Revenue grew (obs-revenue).","evidenceClaims":[{"id":"c1","statement":"Revenue grew.","evidenceRefs":["obs-revenue"]}]}`))
	g.run.Context = finalChecksSnapshotContext(t)
	if _, err := g.exec.Settle(g.ctx, g.context("final", ControlAuthority{FinalMode: FinalModeEvidence})); err == nil {
		t.Fatal("evidence final accepted without a reviewer")
	}
}
