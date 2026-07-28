package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type groundingTurnHost struct {
	responses []*HostedTurnResponse
	reviews   int
	requests  []HostedTurnRequest
}

func (h *groundingTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.requests = append(h.requests, request)
	response := h.responses[0]
	h.responses = h.responses[1:]
	cloned := *response
	cloned.APIVersion = HostedTurnAPIVersion
	cloned.InvocationID = request.InvocationID
	cloned.ModelProvider = "author-provider"
	cloned.Model = "author-model"
	return &cloned, nil
}

func (h *groundingTurnHost) ReviewEvidenceGrounding(_ context.Context, request EvidenceGroundingRequest) (*EvidenceGroundingReview, error) {
	h.reviews++
	findings := make([]EvidenceGroundingFinding, 0, len(request.Claims))
	accepted := true
	for _, claim := range request.Claims {
		status := EvidenceGroundingSupported
		summary := "The cited observation supports this claim."
		if strings.Contains(claim.Statement, "AgentTransfer") && len(claim.EvidenceRefs) == 1 && claim.EvidenceRefs[0] == "observation-firewall" {
			status = EvidenceGroundingUnsupported
			summary = "The cited firewall observation does not support the AgentTransfer claim."
			accepted = false
		}
		findings = append(findings, EvidenceGroundingFinding{ClaimID: claim.ID, Status: status, Summary: summary, EvidenceRefs: claim.EvidenceRefs})
	}
	return &EvidenceGroundingReview{
		APIVersion: EvidenceGroundingAPIVersion, InvocationID: request.InvocationID,
		SnapshotID: request.Snapshot.ID, ClaimsDigest: request.ClaimsDigest,
		ReviewerProvider: "review-provider", ReviewerModel: "review-model",
		Accepted: accepted, CoverageComplete: true, Findings: findings,
		Usage: TurnUsage{InputTokens: 200, OutputTokens: 50},
	}, nil
}

func groundedSnapshot(t *testing.T) (*EvidenceSnapshot, map[string]interface{}) {
	t.Helper()
	snapshot := &EvidenceSnapshot{
		APIVersion: evidenceSnapshotAPIVersion, ProjectID: "project-one",
		SelectedCount: 2, ObservationLimit: 7, SummaryRuneLimit: 600, TotalSummaryRuneLimit: 4200,
		Observations: []EvidenceSnapshotObservation{
			{ID: "observation-transfer", SourceURI: "https://example.test/transfer", ObservedAt: time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC), ContentDigest: "sha256:transfer", Summary: "AgentTransfer loses state while moving work between agents."},
			{ID: "observation-firewall", SourceURI: "https://example.test/firewall", ObservedAt: time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC), ContentDigest: "sha256:firewall", Summary: "Firewall defaults prevent a local connector from starting."},
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
	return snapshot, map[string]interface{}{EvidenceSnapshotContextKey: projection}
}

func TestEvidenceGroundingRejectsSwappedCitationThenAcceptsRepair(t *testing.T) {
	_, runContext := groundedSnapshot(t)
	host := &groundingTurnHost{responses: []*HostedTurnResponse{
		{OutputSummary: "Drafted report", NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"report": "AgentTransfer loses state [observation-firewall]."}, EvidenceClaims: []EvidenceClaim{{ID: "claim-transfer", Statement: "AgentTransfer loses state.", EvidenceRefs: []string{"observation-firewall"}}}},
		{OutputSummary: "Repaired report", NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"report": "AgentTransfer loses state [observation-transfer]."}, EvidenceClaims: []EvidenceClaim{{ID: "claim-transfer", Statement: "AgentTransfer loses state.", EvidenceRefs: []string{"observation-transfer"}}}},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "analyst", DefinitionID: "analyst", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-one", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Synthesize", Context: runContext}

	draft, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-one"}})
	if err != nil || draft.NextRunStatus != AgentRunStatusRunning || draft.RunOutput != nil {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
	run.Checkpoint = draft.ContinuationCheckpoint
	rejected, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-two"}})
	if err != nil || rejected.NextRunStatus != AgentRunStatusRunning || rejected.EvidenceGrounding == nil || rejected.EvidenceGrounding.Accepted {
		t.Fatalf("rejected=%#v err=%v", rejected, err)
	}
	if len(rejected.EvidenceGrounding.Findings) != 1 || rejected.EvidenceGrounding.Findings[0].Status != EvidenceGroundingUnsupported {
		t.Fatalf("findings=%#v", rejected.EvidenceGrounding.Findings)
	}
	rejectedState, err := parseEvidenceGroundingState(rejected.ContinuationCheckpoint)
	if err != nil || rejectedState == nil || rejectedState.LastReview == nil || rejectedState.LastReview.Accepted ||
		rejectedState.LastReview.ReviewerProvider != "review-provider" || rejectedState.LastReview.Usage.InputTokens == 0 {
		t.Fatalf("durable rejected review state=%#v err=%v", rejectedState, err)
	}
	run.Checkpoint = rejected.ContinuationCheckpoint
	repaired, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-three"}})
	if err != nil || repaired.NextRunStatus != AgentRunStatusRunning || repaired.RunOutput != nil {
		t.Fatalf("repair=%#v err=%v", repaired, err)
	}
	run.Checkpoint = repaired.ContinuationCheckpoint
	accepted, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-four"}})
	if err != nil || accepted.NextRunStatus != AgentRunStatusCompleted || accepted.EvidenceGrounding == nil || !accepted.EvidenceGrounding.Accepted {
		t.Fatalf("accepted=%#v err=%v", accepted, err)
	}
	if accepted.RunOutput["report"] != "AgentTransfer loses state [observation-transfer]." || accepted.RunOutput["evidenceClaims"] == nil || accepted.RunOutput["evidenceGrounding"] == nil {
		t.Fatalf("published output=%#v", accepted.RunOutput)
	}
	if _, remains := accepted.ContinuationCheckpoint[evidenceGroundingCheckpointKey]; remains || host.reviews != 2 {
		t.Fatalf("checkpoint=%#v reviews=%d", accepted.ContinuationCheckpoint, host.reviews)
	}
}

func TestEvidenceGroundingRejectsUnknownSnapshotReferenceBeforeReview(t *testing.T) {
	_, runContext := groundedSnapshot(t)
	host := &groundingTurnHost{responses: []*HostedTurnResponse{{
		OutputSummary: "Draft", NextRunStatus: AgentRunStatusCompleted,
		RunOutput:      map[string]interface{}{"report": "Unsupported."},
		EvidenceClaims: []EvidenceClaim{{ID: "claim-one", Statement: "Unsupported.", EvidenceRefs: []string{"observation-invented"}}},
	}}}
	runner, _ := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "analyst", DefinitionID: "analyst", DefinitionVersion: "1"})
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Synthesize", Context: runContext}, Turn: &AgentTurn{ID: "turn"}})
	if err != nil || outcome.NextRunStatus != AgentRunStatusRunning || outcome.RunOutput != nil || outcome.EvidenceGrounding == nil || len(outcome.EvidenceGrounding.Findings) == 0 {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	if host.reviews != 0 || !strings.Contains(outcome.EvidenceGrounding.Findings[0].Summary, "outside the immutable snapshot") {
		t.Fatalf("review count=%d findings=%#v", host.reviews, outcome.EvidenceGrounding.Findings)
	}
}

func TestEvidenceGroundingRequiresCitedHumanReadableReport(t *testing.T) {
	_, runContext := groundedSnapshot(t)
	host := &groundingTurnHost{responses: []*HostedTurnResponse{{
		OutputSummary: "Claims only", NextRunStatus: AgentRunStatusCompleted,
		RunOutput: map[string]interface{}{"summary": "AgentTransfer has friction."},
		EvidenceClaims: []EvidenceClaim{{
			ID: "claim-transfer", Statement: "AgentTransfer loses state.", EvidenceRefs: []string{"observation-transfer"},
		}},
	}}}
	runner, _ := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "analyst", DefinitionID: "analyst", DefinitionVersion: "1"})
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Synthesize", Context: runContext},
		Turn: &AgentTurn{ID: "turn"},
	})
	if err != nil || outcome.NextRunStatus != AgentRunStatusRunning || outcome.RunOutput != nil || outcome.EvidenceGrounding == nil {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	if host.reviews != 0 || len(outcome.EvidenceGrounding.Findings) != 1 || !strings.Contains(outcome.EvidenceGrounding.Findings[0].Summary, "runOutput.report") {
		t.Fatalf("reviews=%d findings=%#v", host.reviews, outcome.EvidenceGrounding.Findings)
	}
	if len(host.requests) != 1 || !containsString(host.requests[0].SystemInstructions, evidenceGroundingDraftInstruction) {
		t.Fatalf("grounding instructions=%#v", host.requests)
	}
}

func TestEvidenceGroundingRequiresEveryClaimCitationInReport(t *testing.T) {
	_, runContext := groundedSnapshot(t)
	host := &groundingTurnHost{responses: []*HostedTurnResponse{{
		OutputSummary: "Draft", NextRunStatus: AgentRunStatusCompleted,
		RunOutput: map[string]interface{}{"report": "AgentTransfer loses state."},
		EvidenceClaims: []EvidenceClaim{{
			ID: "claim-transfer", Statement: "AgentTransfer loses state.", EvidenceRefs: []string{"observation-transfer"},
		}},
	}}}
	runner, _ := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "analyst", DefinitionID: "analyst", DefinitionVersion: "1"})
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Synthesize", Context: runContext},
		Turn: &AgentTurn{ID: "turn"},
	})
	if err != nil || outcome.EvidenceGrounding == nil || len(outcome.EvidenceGrounding.Findings) != 1 {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	finding := outcome.EvidenceGrounding.Findings[0]
	if finding.ClaimID != "claim-transfer" || len(finding.EvidenceRefs) != 1 || finding.EvidenceRefs[0] != "observation-transfer" {
		t.Fatalf("citation finding=%#v", finding)
	}
}
