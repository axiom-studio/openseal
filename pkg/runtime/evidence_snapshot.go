package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

const (
	EvidenceSnapshotContextKey = "evidenceSnapshot"
	evidenceSnapshotAPIVersion = "openseal.evidence-snapshot/v1"
	evidenceSnapshotRefPrefix  = "evidence-snapshot:"

	defaultEvidenceMaximumObservations = 25
	defaultEvidenceMaximumSummaryRunes = 1000
	defaultEvidenceMaximumTotalRunes   = 20000
)

// EvidenceSnapshot is a bounded, credential-free, immutable projection of
// canonical SourceObservations. It intentionally excludes source metadata,
// raw content, artifacts, action inputs, and credential bindings.
type EvidenceSnapshot struct {
	APIVersion            string                        `json:"apiVersion"`
	ID                    string                        `json:"id"`
	ProjectID             string                        `json:"projectId"`
	Observations          []EvidenceSnapshotObservation `json:"observations"`
	SelectedCount         int                           `json:"selectedCount"`
	ExpiredCount          int                           `json:"expiredCount,omitempty"`
	Truncated             bool                          `json:"truncated"`
	ObservationLimit      int                           `json:"observationLimit"`
	SummaryRuneLimit      int                           `json:"summaryRuneLimit"`
	TotalSummaryRuneLimit int                           `json:"totalSummaryRuneLimit"`
}

type EvidenceSnapshotObservation struct {
	ID               string    `json:"id"`
	SourceURI        string    `json:"sourceUri"`
	ObservedAt       time.Time `json:"observedAt"`
	ContentDigest    string    `json:"contentDigest"`
	Summary          string    `json:"summary"`
	SummaryTruncated bool      `json:"summaryTruncated"`
}

type evidenceProjectionBounds struct {
	observations int
	summaryRunes int
	totalRunes   int
}

func normalizedEvidenceProjection(value *runbook.EvidenceProjection) (evidenceProjectionBounds, bool) {
	if value != nil && value.Disabled {
		return evidenceProjectionBounds{}, false
	}
	bounds := evidenceProjectionBounds{
		observations: defaultEvidenceMaximumObservations,
		summaryRunes: defaultEvidenceMaximumSummaryRunes,
		totalRunes:   defaultEvidenceMaximumTotalRunes,
	}
	if value != nil {
		if value.MaximumObservations > 0 {
			bounds.observations = value.MaximumObservations
		}
		if value.MaximumSummaryRunes > 0 {
			bounds.summaryRunes = value.MaximumSummaryRunes
		}
		if value.MaximumTotalRunes > 0 {
			bounds.totalRunes = value.MaximumTotalRunes
		}
	}
	return bounds, true
}

func buildEvidenceSnapshot(ctx context.Context, store SourceMonitorStore, scope Scope, projectID string, cutoff time.Time, projection *runbook.EvidenceProjection) (*EvidenceSnapshot, error) {
	if store == nil {
		return nil, errors.New("Project evidence projection requires SourceObservation persistence")
	}
	bounds, enabled := normalizedEvidenceProjection(projection)
	if !enabled {
		return nil, nil
	}
	// Scan at most the portable SourceObservation page bound. Hitting that bound
	// is reported as truncation rather than loading an unbounded evidence corpus.
	values, err := store.ListSourceObservations(ctx, SourceObservationFilter{
		Scope: scope, ProjectID: projectID, Limit: 100,
	})
	if err != nil {
		return nil, fmt.Errorf("list Project evidence: %w", err)
	}
	sort.SliceStable(values, func(i, j int) bool {
		if !values[i].IngestedAt.Equal(values[j].IngestedAt) {
			return values[i].IngestedAt.After(values[j].IngestedAt)
		}
		return values[i].ID > values[j].ID
	})
	snapshot := &EvidenceSnapshot{
		APIVersion: evidenceSnapshotAPIVersion, ProjectID: projectID,
		ObservationLimit: bounds.observations, SummaryRuneLimit: bounds.summaryRunes,
		TotalSummaryRuneLimit: bounds.totalRunes,
		Observations:          make([]EvidenceSnapshotObservation, 0, min(bounds.observations, len(values))),
	}
	if len(values) == 100 {
		snapshot.Truncated = true
	}
	remaining := bounds.totalRunes
	for _, value := range values {
		if value == nil {
			continue
		}
		if value.ProjectID != projectID || value.Scope != scope {
			return nil, errors.New("SourceObservation query returned evidence outside the Project scope")
		}
		if value.RetentionExpiresAt != nil && !value.RetentionExpiresAt.After(cutoff) {
			snapshot.ExpiredCount++
			continue
		}
		if len(snapshot.Observations) == bounds.observations {
			snapshot.Truncated = true
			break
		}
		summaryRunes := []rune(strings.TrimSpace(value.Summary))
		limit := min(bounds.summaryRunes, remaining)
		truncated := len(summaryRunes) > limit
		if truncated {
			summaryRunes = summaryRunes[:limit]
		}
		if limit == 0 {
			snapshot.Truncated = true
			break
		}
		snapshot.Observations = append(snapshot.Observations, EvidenceSnapshotObservation{
			ID: value.ID, SourceURI: value.SourceURI, ObservedAt: value.ObservedAt.UTC(),
			ContentDigest: value.ContentDigest, Summary: string(summaryRunes), SummaryTruncated: truncated,
		})
		remaining -= len(summaryRunes)
		if truncated {
			snapshot.Truncated = true
		}
	}
	snapshot.SelectedCount = len(snapshot.Observations)
	identity := *snapshot
	identity.ID = ""
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode evidence snapshot identity: %w", err)
	}
	snapshot.ID = hashBytes(encoded)
	return snapshot, nil
}

func evidenceSnapshotContext(snapshot *EvidenceSnapshot) (map[string]interface{}, error) {
	if snapshot == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var projected map[string]interface{}
	if err := json.Unmarshal(encoded, &projected); err != nil {
		return nil, err
	}
	return projected, ValidateCredentialFreeContext(projected)
}

func evidenceSnapshotIdentity(contextValues map[string]interface{}) string {
	value, ok := contextValues[EvidenceSnapshotContextKey].(map[string]interface{})
	if !ok {
		return ""
	}
	id, _ := value["id"].(string)
	return strings.TrimSpace(id)
}

func evidenceSnapshotInputContextRef(contextValues map[string]interface{}) string {
	id := evidenceSnapshotIdentity(contextValues)
	if id == "" {
		return ""
	}
	return evidenceSnapshotRefPrefix + id
}
