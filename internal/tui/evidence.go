package tui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/charmbracelet/lipgloss"
)

const evidenceSnapshotAPIVersion = "openseal.evidence-snapshot/v1"

const (
	evidenceGroundingAPIVersion = "openseal.evidence-grounding/v1"
	evidenceGroundingCheckpoint = "_opensealEvidenceGrounding"
	groundingPendingReview      = "pending_review"
	groundingRepairRequired     = "repair_required"
)

type evidenceLineage struct {
	run      *runtime.AgentRun
	snapshot *runtime.EvidenceSnapshot
	err      error
}

// decodeEvidenceSnapshot only accepts the exact portable snapshot contract.
// Unknown fields are rejected so an API mismatch cannot turn arbitrary Run
// context (including future sensitive fields) into TUI output.
func decodeEvidenceSnapshot(contextValues map[string]interface{}) (*runtime.EvidenceSnapshot, bool, error) {
	value, present := contextValues[runtime.EvidenceSnapshotContextKey]
	if !present {
		return nil, false, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, true, errors.New("evidence snapshot is not a portable projection")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var snapshot runtime.EvidenceSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, true, errors.New("evidence snapshot contract does not match this TUI")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, true, errors.New("evidence snapshot contract contains multiple values")
	}
	if snapshot.APIVersion != evidenceSnapshotAPIVersion || strings.TrimSpace(snapshot.ID) == "" ||
		strings.TrimSpace(snapshot.InitiativeID) == "" || snapshot.SelectedCount != len(snapshot.Observations) ||
		snapshot.ObservationLimit < snapshot.SelectedCount || snapshot.ObservationLimit < 1 ||
		snapshot.SummaryRuneLimit < 1 || snapshot.TotalSummaryRuneLimit < 1 {
		return nil, true, errors.New("evidence snapshot invariants are invalid")
	}
	totalSummaryRunes := 0
	for _, observation := range snapshot.Observations {
		totalSummaryRunes += utf8.RuneCountInString(observation.Summary)
		if strings.TrimSpace(observation.ID) == "" || strings.TrimSpace(observation.SourceURI) == "" ||
			observation.ObservedAt.IsZero() || strings.TrimSpace(observation.ContentDigest) == "" ||
			strings.TrimSpace(observation.Summary) == "" || utf8.RuneCountInString(observation.Summary) > snapshot.SummaryRuneLimit {
			return nil, true, errors.New("evidence snapshot observation invariants are invalid")
		}
	}
	if totalSummaryRunes > snapshot.TotalSummaryRuneLimit {
		return nil, true, errors.New("evidence snapshot total summary bound is invalid")
	}
	return &snapshot, true, nil
}

func (m *Model) selectedEvidenceLineage() *evidenceLineage {
	if !m.runCapability.Available || !m.supportsRun(kernelapi.OperationList) {
		return nil
	}
	switch m.section {
	case sectionRuns:
		return evidenceLineageForRun(m.selectedRun())
	case sectionObjectives:
		objective := m.selectedObjectiveRecord()
		if objective == nil {
			return nil
		}
		return latestEvidenceLineage(m.runs, func(run *runtime.AgentRun, _ *runtime.EvidenceSnapshot) bool {
			return run.ObjectiveID == objective.ID
		})
	case sectionInitiatives:
		initiative := m.selectedInitiativeRecord()
		if initiative == nil {
			return nil
		}
		return latestEvidenceLineage(m.runs, func(run *runtime.AgentRun, snapshot *runtime.EvidenceSnapshot) bool {
			if snapshot != nil {
				return snapshot.InitiativeID == initiative.ID
			}
			initiativeID, _ := run.Context["initiativeId"].(string)
			return initiativeID == initiative.ID
		})
	default:
		return nil
	}
}

func evidenceLineageForRun(run *runtime.AgentRun) *evidenceLineage {
	if run == nil {
		return nil
	}
	snapshot, present, err := decodeEvidenceSnapshot(run.Context)
	if !present {
		return nil
	}
	return &evidenceLineage{run: run, snapshot: snapshot, err: err}
}

func latestEvidenceLineage(runs []*runtime.AgentRun, matches func(*runtime.AgentRun, *runtime.EvidenceSnapshot) bool) *evidenceLineage {
	var selected *evidenceLineage
	for _, run := range runs {
		candidate := evidenceLineageForRun(run)
		if candidate == nil || !matches(run, candidate.snapshot) {
			continue
		}
		if selected == nil || evidenceRunTime(run).After(evidenceRunTime(selected.run)) ||
			(evidenceRunTime(run).Equal(evidenceRunTime(selected.run)) && run.ID > selected.run.ID) {
			selected = candidate
		}
	}
	return selected
}

func evidenceRunTime(run *runtime.AgentRun) time.Time {
	if run == nil {
		return time.Time{}
	}
	if !run.CreatedAt.IsZero() {
		return run.CreatedAt
	}
	return run.UpdatedAt
}

func (m *Model) resetEvidenceInspection() {
	m.evidenceExpanded = false
	m.evidenceObservationSelected = 0
	m.groundingExpanded = false
	m.groundingPageSelected = 0
}

func (m *Model) moveEvidenceObservation(delta int) {
	lineage := m.selectedEvidenceLineage()
	if !m.evidenceExpanded || lineage == nil || lineage.snapshot == nil || len(lineage.snapshot.Observations) == 0 {
		return
	}
	m.evidenceObservationSelected = max(0, min(len(lineage.snapshot.Observations)-1, m.evidenceObservationSelected+delta))
}

func (m *Model) renderSelectedEvidence(width int) []string {
	lineage := m.selectedEvidenceLineage()
	if lineage == nil {
		return nil
	}
	lines := []string{"", headerStyle.Render("Evidence snapshot")}
	if lineage.err != nil || lineage.snapshot == nil {
		return append(lines,
			lipgloss.NewStyle().Foreground(danger).Render("Snapshot projection unavailable · "+lineage.err.Error()),
			mutedStyle.Render("Run "+lineage.run.ID+" · refresh after upgrading the kernel or TUI"),
		)
	}
	snapshot := lineage.snapshot
	state := fmt.Sprintf("%d selected · %d expired", snapshot.SelectedCount, snapshot.ExpiredCount)
	if snapshot.Truncated {
		state += " · bounded/truncated"
	} else {
		state += " · complete within bounds"
	}
	lines = append(lines, mutedStyle.Render(state))
	lines = appendEvidenceField(lines, "Snapshot", snapshot.ID, width)
	lines = appendEvidenceField(lines, "Initiative", snapshot.InitiativeID, width)
	lines = appendEvidenceField(lines, "Run", lineage.run.ID, width)
	lines = append(lines, mutedStyle.Render(fmt.Sprintf("Bounds · %d observations · %d runes/summary · %d runes total", snapshot.ObservationLimit, snapshot.SummaryRuneLimit, snapshot.TotalSummaryRuneLimit)))
	if !m.evidenceExpanded {
		return append(lines, lipgloss.NewStyle().Foreground(accentSoft).Render("v expand bounded evidence"))
	}
	if len(snapshot.Observations) == 0 {
		return append(lines, mutedStyle.Render("No retained observations were selected."), lipgloss.NewStyle().Foreground(accentSoft).Render("v collapse"))
	}
	selected := max(0, min(len(snapshot.Observations)-1, m.evidenceObservationSelected))
	observation := snapshot.Observations[selected]
	lines = append(lines, "", fmt.Sprintf("Observation %d of %d", selected+1, len(snapshot.Observations)))
	lines = appendEvidenceField(lines, "ID", observation.ID, width)
	lines = appendEvidenceField(lines, "Source", observation.SourceURI, width)
	lines = appendEvidenceField(lines, "Observed", observation.ObservedAt.UTC().Format(time.RFC3339Nano), width)
	lines = appendEvidenceField(lines, "Digest", observation.ContentDigest, width)
	summaryLabel := "Summary"
	if observation.SummaryTruncated {
		summaryLabel += " (bounded)"
	}
	lines = appendEvidenceField(lines, summaryLabel, observation.Summary, width)
	return append(lines, lipgloss.NewStyle().Foreground(accentSoft).Render("[ previous · ] next · v collapse"))
}

type evidenceGroundingCheckpointState struct {
	APIVersion   string                             `json:"apiVersion"`
	Status       string                             `json:"status"`
	SnapshotID   string                             `json:"snapshotId"`
	ClaimsDigest string                             `json:"claimsDigest"`
	Claims       []runtime.EvidenceClaim            `json:"claims"`
	DraftSummary string                             `json:"draftSummary"`
	DraftOutput  map[string]interface{}             `json:"draftOutput"`
	Findings     []runtime.EvidenceGroundingFinding `json:"findings,omitempty"`
	LastReview   *runtime.EvidenceGroundingReview   `json:"lastReview,omitempty"`
}

type evidenceGroundingProjection struct {
	run          *runtime.AgentRun
	phase        string
	snapshotID   string
	claimsDigest string
	claims       []runtime.EvidenceClaim
	review       *runtime.EvidenceGroundingReview
	findings     []runtime.EvidenceGroundingFinding
	err          error
}

type evidenceGroundingPage struct {
	claim    *runtime.EvidenceClaim
	findings []runtime.EvidenceGroundingFinding
}

func (m *Model) selectedEvidenceGrounding() *evidenceGroundingProjection {
	lineage := m.selectedEvidenceLineage()
	if lineage == nil || lineage.err != nil || lineage.snapshot == nil {
		return nil
	}
	projection, present, err := decodeEvidenceGrounding(lineage.run, lineage.snapshot)
	if !present {
		return nil
	}
	if err != nil {
		return &evidenceGroundingProjection{run: lineage.run, err: err}
	}
	return projection
}

func decodeEvidenceGrounding(run *runtime.AgentRun, snapshot *runtime.EvidenceSnapshot) (*evidenceGroundingProjection, bool, error) {
	if run == nil || snapshot == nil {
		return nil, false, nil
	}
	claimsValue, claimsPresent := run.Output["evidenceClaims"]
	reviewValue, reviewPresent := run.Output["evidenceGrounding"]
	checkpointValue, checkpointPresent := run.Checkpoint[evidenceGroundingCheckpoint]
	if !claimsPresent && !reviewPresent && !checkpointPresent {
		return nil, false, nil
	}
	if claimsPresent != reviewPresent || (claimsPresent && checkpointPresent) {
		return nil, true, errors.New("Run contains an ambiguous evidence grounding projection")
	}
	if claimsPresent {
		var claims []runtime.EvidenceClaim
		if err := decodeStrictProjection(claimsValue, &claims); err != nil {
			return nil, true, errors.New("evidence claim contract does not match this TUI")
		}
		var review runtime.EvidenceGroundingReview
		if err := decodeStrictProjection(reviewValue, &review); err != nil {
			return nil, true, errors.New("evidence review contract does not match this TUI")
		}
		if run.Status != runtime.AgentRunStatusCompleted || !review.Accepted || !review.CoverageComplete {
			return nil, true, errors.New("completed evidence grounding verdict is invalid")
		}
		if err := validateEvidenceGroundingProjection(claims, &review, snapshot, review.ClaimsDigest); err != nil {
			return nil, true, err
		}
		return &evidenceGroundingProjection{
			run: run, phase: "accepted", snapshotID: review.SnapshotID, claimsDigest: review.ClaimsDigest,
			claims: claims, review: &review, findings: review.Findings,
		}, true, nil
	}

	var state evidenceGroundingCheckpointState
	if err := decodeStrictProjection(checkpointValue, &state); err != nil {
		return nil, true, errors.New("evidence grounding checkpoint contract does not match this TUI")
	}
	if state.APIVersion != evidenceGroundingAPIVersion ||
		(state.Status != groundingPendingReview && state.Status != groundingRepairRequired) ||
		state.SnapshotID != snapshot.ID || strings.TrimSpace(state.ClaimsDigest) == "" {
		return nil, true, errors.New("evidence grounding checkpoint invariants are invalid")
	}
	if err := validateEvidenceClaims(state.Claims, snapshot, state.ClaimsDigest); err != nil {
		return nil, true, err
	}
	findings := state.Findings
	if state.LastReview != nil {
		if state.Status != groundingRepairRequired || state.LastReview.Accepted || state.LastReview.SnapshotID != state.SnapshotID || state.LastReview.ClaimsDigest != state.ClaimsDigest {
			return nil, true, errors.New("in-progress evidence review envelope is invalid")
		}
		if err := validateEvidenceGroundingProjection(state.Claims, state.LastReview, snapshot, state.ClaimsDigest); err != nil {
			return nil, true, err
		}
		if !equalProjection(state.Findings, state.LastReview.Findings) {
			return nil, true, errors.New("evidence grounding checkpoint findings do not match the last review")
		}
		findings = state.LastReview.Findings
	} else {
		if state.Status == groundingPendingReview && len(findings) != 0 {
			return nil, true, errors.New("pending evidence review cannot contain findings")
		}
		if err := validatePendingGroundingFindings(findings, state.Claims, snapshot); err != nil {
			return nil, true, err
		}
	}
	return &evidenceGroundingProjection{
		run: run, phase: state.Status, snapshotID: state.SnapshotID, claimsDigest: state.ClaimsDigest,
		claims: state.Claims, review: state.LastReview, findings: findings,
	}, true, nil
}

func decodeStrictProjection(value interface{}, target interface{}) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("projection contains multiple values")
	}
	return nil
}

func validateEvidenceClaims(claims []runtime.EvidenceClaim, snapshot *runtime.EvidenceSnapshot, digest string) error {
	if len(claims) == 0 {
		return errors.New("evidence grounding contains no claims")
	}
	allowedEvidence := make(map[string]struct{}, len(snapshot.Observations))
	for _, observation := range snapshot.Observations {
		allowedEvidence[observation.ID] = struct{}{}
	}
	seenClaims := make(map[string]struct{}, len(claims))
	for _, claim := range claims {
		if strings.TrimSpace(claim.ID) == "" || strings.TrimSpace(claim.Statement) == "" || len(claim.EvidenceRefs) == 0 {
			return errors.New("evidence claim invariants are invalid")
		}
		if _, duplicate := seenClaims[claim.ID]; duplicate {
			return errors.New("evidence grounding contains duplicate claims")
		}
		seenClaims[claim.ID] = struct{}{}
		seenRefs := make(map[string]struct{}, len(claim.EvidenceRefs))
		for _, reference := range claim.EvidenceRefs {
			if _, allowed := allowedEvidence[reference]; !allowed {
				return errors.New("evidence claim references an observation outside the immutable snapshot")
			}
			if _, duplicate := seenRefs[reference]; duplicate {
				return errors.New("evidence claim contains duplicate observation references")
			}
			seenRefs[reference] = struct{}{}
		}
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		return errors.New("evidence claims cannot be encoded")
	}
	computed := sha256.Sum256(encoded)
	if "sha256:"+hex.EncodeToString(computed[:]) != digest {
		return errors.New("evidence claim digest does not match its contents")
	}
	return nil
}

func validateEvidenceGroundingProjection(claims []runtime.EvidenceClaim, review *runtime.EvidenceGroundingReview, snapshot *runtime.EvidenceSnapshot, digest string) error {
	if err := validateEvidenceClaims(claims, snapshot, digest); err != nil {
		return err
	}
	if review == nil || review.APIVersion != evidenceGroundingAPIVersion || strings.TrimSpace(review.InvocationID) == "" ||
		review.SnapshotID != snapshot.ID || review.ClaimsDigest != digest || strings.TrimSpace(review.ReviewerProvider) == "" ||
		strings.TrimSpace(review.ReviewerModel) == "" || review.Usage.Validate() != nil {
		return errors.New("evidence review invariants are invalid")
	}
	claimByID := make(map[string]runtime.EvidenceClaim, len(claims))
	for _, claim := range claims {
		claimByID[claim.ID] = claim
	}
	seen := make(map[string]struct{}, len(review.Findings))
	allSupported := true
	for _, finding := range review.Findings {
		claim, known := claimByID[finding.ClaimID]
		if !known || strings.TrimSpace(finding.Summary) == "" {
			return errors.New("evidence review contains an unknown or incomplete finding")
		}
		if _, duplicate := seen[finding.ClaimID]; duplicate {
			return errors.New("evidence review contains duplicate claim findings")
		}
		seen[finding.ClaimID] = struct{}{}
		if finding.Status != runtime.EvidenceGroundingSupported && finding.Status != runtime.EvidenceGroundingUnsupported && finding.Status != runtime.EvidenceGroundingUncertain {
			return errors.New("evidence review contains an unsupported finding status")
		}
		allowedRefs := make(map[string]struct{}, len(claim.EvidenceRefs))
		for _, reference := range claim.EvidenceRefs {
			allowedRefs[reference] = struct{}{}
		}
		for _, reference := range finding.EvidenceRefs {
			if _, allowed := allowedRefs[reference]; !allowed {
				return errors.New("evidence review references evidence outside its claim")
			}
		}
		if finding.Status != runtime.EvidenceGroundingSupported {
			allSupported = false
		}
	}
	allClaimsReviewed := len(seen) == len(claims)
	if review.Accepted != (review.CoverageComplete && allClaimsReviewed && allSupported) {
		return errors.New("evidence review verdict contradicts its findings or coverage")
	}
	return nil
}

func validatePendingGroundingFindings(findings []runtime.EvidenceGroundingFinding, claims []runtime.EvidenceClaim, snapshot *runtime.EvidenceSnapshot) error {
	claimIDs := make(map[string]struct{}, len(claims))
	for _, claim := range claims {
		claimIDs[claim.ID] = struct{}{}
	}
	evidenceIDs := make(map[string]struct{}, len(snapshot.Observations))
	for _, observation := range snapshot.Observations {
		evidenceIDs[observation.ID] = struct{}{}
	}
	for _, finding := range findings {
		if strings.TrimSpace(finding.Summary) == "" ||
			(finding.Status != runtime.EvidenceGroundingUnsupported && finding.Status != runtime.EvidenceGroundingUncertain) {
			return errors.New("pending evidence grounding finding is invalid")
		}
		if finding.ClaimID != "" {
			if _, known := claimIDs[finding.ClaimID]; !known {
				return errors.New("pending evidence grounding finding names an unknown claim")
			}
		}
		for _, reference := range finding.EvidenceRefs {
			if _, known := evidenceIDs[reference]; !known {
				return errors.New("pending evidence grounding finding names evidence outside the snapshot")
			}
		}
	}
	return nil
}

func equalProjection(left, right interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func groundingPages(projection *evidenceGroundingProjection) []evidenceGroundingPage {
	if projection == nil {
		return nil
	}
	pages := make([]evidenceGroundingPage, 0, len(projection.claims)+len(projection.findings))
	usedFindings := make([]bool, len(projection.findings))
	for claimIndex := range projection.claims {
		claim := &projection.claims[claimIndex]
		page := evidenceGroundingPage{claim: claim}
		for findingIndex, finding := range projection.findings {
			if finding.ClaimID == claim.ID {
				page.findings = append(page.findings, finding)
				usedFindings[findingIndex] = true
			}
		}
		pages = append(pages, page)
	}
	for index, finding := range projection.findings {
		if !usedFindings[index] {
			pages = append(pages, evidenceGroundingPage{findings: []runtime.EvidenceGroundingFinding{finding}})
		}
	}
	return pages
}

func (m *Model) moveGroundingPage(delta int) {
	grounding := m.selectedEvidenceGrounding()
	pages := groundingPages(grounding)
	if !m.groundingExpanded || grounding == nil || grounding.err != nil || len(pages) == 0 {
		return
	}
	m.groundingPageSelected = max(0, min(len(pages)-1, m.groundingPageSelected+delta))
}

func (m *Model) renderSelectedEvidenceGrounding(width int) []string {
	grounding := m.selectedEvidenceGrounding()
	if grounding == nil {
		return nil
	}
	lines := []string{"", headerStyle.Render("Evidence grounding")}
	if grounding.err != nil {
		return append(lines,
			lipgloss.NewStyle().Foreground(danger).Render("Review projection unavailable · "+grounding.err.Error()),
			mutedStyle.Render("Run "+grounding.run.ID+" · refresh after upgrading the kernel or TUI"),
		)
	}
	status := "REVIEW PENDING"
	statusStyle := lipgloss.NewStyle().Foreground(accentSoft).Bold(true)
	if grounding.phase == "accepted" {
		status = "VERIFIED"
		statusStyle = lipgloss.NewStyle().Foreground(success).Bold(true)
	} else if grounding.phase == groundingRepairRequired {
		status = "REPAIR REQUIRED"
		statusStyle = lipgloss.NewStyle().Foreground(danger).Bold(true)
	}
	lines = append(lines, statusStyle.Render(status)+mutedStyle.Render(fmt.Sprintf(" · %d claims", len(grounding.claims))))
	accepted, coverage := "no", "incomplete"
	if grounding.review != nil && grounding.review.Accepted {
		accepted = "yes"
	}
	if grounding.review != nil && grounding.review.CoverageComplete {
		coverage = "complete"
	}
	lines = append(lines, mutedStyle.Render("Accepted · "+accepted+" · Coverage · "+coverage))
	lines = appendEvidenceField(lines, "Snapshot", grounding.snapshotID, width)
	lines = appendEvidenceField(lines, "Claims digest", grounding.claimsDigest, width)
	if grounding.review == nil {
		lines = append(lines, mutedStyle.Render("Reviewer · awaiting independent review"), mutedStyle.Render("Usage · not recorded yet"))
	} else {
		lines = appendEvidenceField(lines, "Reviewer", grounding.review.ReviewerProvider+" / "+grounding.review.ReviewerModel, width)
		usage := grounding.review.Usage
		lines = appendEvidenceField(lines, "Usage", fmt.Sprintf("%d input · %d output · %.6g cost · %d ms", usage.InputTokens, usage.OutputTokens, usage.Cost, usage.DurationMS), width)
	}
	pages := groundingPages(grounding)
	if !m.groundingExpanded {
		return append(lines, lipgloss.NewStyle().Foreground(accentSoft).Render(fmt.Sprintf("V expand %d claim/review pages", len(pages))))
	}
	if len(pages) == 0 {
		return append(lines, mutedStyle.Render("No claim or finding detail is available."), lipgloss.NewStyle().Foreground(accentSoft).Render("V collapse"))
	}
	selected := max(0, min(len(pages)-1, m.groundingPageSelected))
	page := pages[selected]
	lines = append(lines, "", fmt.Sprintf("Claim/review page %d of %d", selected+1, len(pages)))
	if page.claim != nil {
		lines = appendEvidenceField(lines, "Claim", page.claim.ID, width)
		lines = appendEvidenceField(lines, "Statement", page.claim.Statement, width)
		for index, reference := range page.claim.EvidenceRefs {
			lines = appendEvidenceField(lines, fmt.Sprintf("Evidence ref %d", index+1), reference, width)
		}
	}
	for index, finding := range page.findings {
		label := fmt.Sprintf("Finding %d", index+1)
		lines = appendEvidenceField(lines, label, string(finding.Status)+" · "+finding.Summary, width)
		for refIndex, reference := range finding.EvidenceRefs {
			lines = appendEvidenceField(lines, fmt.Sprintf("Reviewed ref %d", refIndex+1), reference, width)
		}
	}
	return append(lines, lipgloss.NewStyle().Foreground(accentSoft).Render("{ previous · } next · V collapse"))
}

func appendEvidenceField(lines []string, label, value string, width int) []string {
	available := max(width-8, 24)
	prefix := label + " · "
	firstWidth := max(8, available-utf8.RuneCountInString(prefix))
	chunks := runeChunks(value, firstWidth, available)
	if len(chunks) == 0 {
		return append(lines, mutedStyle.Render(prefix))
	}
	lines = append(lines, mutedStyle.Render(prefix+chunks[0]))
	for _, chunk := range chunks[1:] {
		lines = append(lines, mutedStyle.Render("  "+chunk))
	}
	return lines
}

func runeChunks(value string, firstLimit, remainingLimit int) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	remaining := []rune(value)
	chunks := make([]string, 0, 2)
	limit := max(firstLimit, 1)
	for len(remaining) > 0 {
		count := min(limit, len(remaining))
		chunks = append(chunks, string(remaining[:count]))
		remaining = remaining[count:]
		limit = max(remainingLimit, 1)
	}
	return chunks
}
