package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const conversationArtifactReceiptService = "openseal.artifact-delivery"

// runActionArtifactReferences returns the artifact references of run's
// succeeded governed tool calls, in entry order, deduplicated by (id,
// version). They ride on the Run's final answer. A Run with no session log
// (or stores that cannot read it) has none.
func runActionArtifactReferences(ctx context.Context, sessions loopSessionEntryLister, actions actionCallReader, run *AgentRun) ([]savedArtifact, error) {
	if sessions == nil || actions == nil || run == nil || run.ID == "" {
		return nil, nil
	}
	entries, err := sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, States: []ToolEntryState{ToolEntryCompleted}, Limit: maxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	var references []savedArtifact
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if entry == nil || entry.ActionCallID == "" {
			continue
		}
		call, err := actions.GetActionCall(ctx, run.Scope, entry.ActionCallID)
		if errors.Is(err, ErrActionNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if call == nil || call.RunID != run.ID || call.Status != ActionCallStatusSucceeded {
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

// savedArtifact is a file a tool saved, with the chat path it was saved from
// when the tool reported one, so an answer can hand it over by either.
type savedArtifact struct {
	Ref  ConversationReference
	Path string
}

// actionCallArtifactReferences parses call.Output["artifactRefs"]: valid
// opaque ids of at most 256 bytes with a version of at least 1; anything
// else is skipped.
func actionCallArtifactReferences(call *ActionCall) []savedArtifact {
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
	references := make([]savedArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		id := strings.TrimSpace(artifact.ID)
		if !validOpaqueIdentifier(id, 256) || artifact.Version < 1 {
			continue
		}
		references = append(references, savedArtifact{
			Ref:  ConversationReference{Kind: ConversationReferenceArtifact, ID: id, Version: artifact.Version},
			Path: handedOverPath(artifact.Path),
		})
	}
	return references
}
