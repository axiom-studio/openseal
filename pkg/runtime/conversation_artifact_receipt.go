package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/computer"
	"github.com/axiom-studio/openseal/pkg/models"
)

const conversationArtifactReceiptService = "openseal.artifact-delivery"

// runActionArtifactReferences returns the artifact references of run's
// succeeded governed tool calls, in entry order, deduplicated by (id,
// version). They ride on the Run's final answer. A Run with no session log
// (or stores that cannot read it) has none.
func runActionArtifactReferences(ctx context.Context, sessions loopSessionEntryLister, actions ActionCallReader, run *AgentRun) ([]savedArtifact, error) {
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
		shown := actionShowsInChat(call)
		for _, ref := range actionCallArtifactReferences(call) {
			ref.Shown = shown
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
//
// Shown is set for a file the Agent showed in the chat itself (save_app,
// present): it goes out with the answer whether or not the answer names it.
type savedArtifact struct {
	Ref   models.ConversationReference
	Path  string
	Shown bool
}

// actionShowsInChat reports whether call is one whose purpose is to show its
// files in the conversation: saving an app ("build it and show it live") or
// presenting files. A model that made one has handed the result over; when
// the answer had to repeat the id, an answer that did not left the person
// with "the app is above" and nothing above.
func actionShowsInChat(call *ActionCall) bool {
	if call == nil {
		return false
	}
	switch call.SkillID {
	case builtin.SkillID:
		return call.Action == builtin.Present || call.Action == builtin.SaveApp
	case computer.SkillID:
		return call.Action == computer.Present
	}
	return false
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
