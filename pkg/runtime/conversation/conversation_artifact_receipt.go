package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

const ArtifactReceiptService = "openseal.artifact-delivery"

// RunActionArtifactReferences returns the artifact references of run's
// succeeded governed tool calls, in entry order, deduplicated by (id,
// version): the files the Run's tools saved, which its answer may link. A
// Run with no session log (or stores that cannot read it) has none.
func RunActionArtifactReferences(ctx context.Context, sessions LoopSessionEntryLister, actions models.ActionCallReader, run *models.AgentRun) ([]models.SavedArtifact, error) {
	if sessions == nil || actions == nil || run == nil || run.ID == "" {
		return nil, nil
	}
	entries, err := sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, States: []models.ToolEntryState{models.ToolEntryCompleted}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	var references []models.SavedArtifact
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if entry == nil || entry.ActionCallID == "" {
			continue
		}
		call, err := actions.GetActionCall(ctx, run.Scope, entry.ActionCallID)
		if errors.Is(err, models.ErrActionNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if call == nil || call.RunID != run.ID || call.Status != models.ActionCallStatusSucceeded {
			continue
		}
		for _, ref := range actionCallArtifactReferences(call) {
			key := fmt.Sprintf("%s\x00%d", ref.Ref.ID, ref.Ref.Version)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			references = append(references, ref)
		}
	}
	return references, nil
}

// actionCallArtifactReferences parses call.Output["artifactRefs"]: valid
// opaque ids of at most 256 bytes with a version of at least 1; anything
// else is skipped.
func actionCallArtifactReferences(call *models.ActionCall) []models.SavedArtifact {
	if call == nil || call.Output == nil {
		return nil
	}
	raw, ok := call.Output["artifactRefs"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var artifacts []struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Path    string `json:"path"`
	}
	if json.Unmarshal(encoded, &artifacts) != nil {
		return nil
	}
	references := make([]models.SavedArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		id := strings.TrimSpace(artifact.ID)
		if !models.IsValidOpaqueIdentifier(id, 256) || artifact.Version < 1 {
			continue
		}
		references = append(references, models.SavedArtifact{
			Ref:  models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: id, Version: artifact.Version},
			Path: savedFilePath(artifact.Path),
		})
	}
	return references
}

// savedFilePath is a chat path as a tool may return it: "docs/a.pdf",
// "/docs/a.pdf" or "./docs/a.pdf" all name the same file.
func savedFilePath(name string) string {
	name = strings.TrimSpace(name)
	for strings.HasPrefix(name, "./") {
		name = name[2:]
	}
	return strings.TrimLeft(name, "/")
}
