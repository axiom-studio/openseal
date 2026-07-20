package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/charmbracelet/lipgloss"
)

const evidenceSnapshotAPIVersion = "openseal.evidence-snapshot/v1"

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
	if run == nil || run.Source != runtime.RunSourceSchedule {
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
