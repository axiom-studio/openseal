package runtime

import (
	"context"
	"errors"
	"strings"
)

// ArtifactPlacement records where an artifact was made: the chat whose run
// produced it and the space that chat lives in. Either reader may be nil;
// placement then fills only what it can.
type ArtifactPlacement struct {
	Runs          ArtifactRunReader
	Conversations ArtifactConversationReader
}

// NewStoreArtifactPlacement places artifacts from whichever run and
// conversation lookups the store supports.
func NewStoreArtifactPlacement(store any) ArtifactPlacement {
	var placement ArtifactPlacement
	if runs, ok := store.(ArtifactRunReader); ok {
		placement.Runs = runs
	}
	if conversations, ok := store.(ArtifactConversationReader); ok {
		placement.Conversations = conversations
	}
	return placement
}

// Place fills Provenance.ConversationID and Provenance.SpaceID when they are
// empty. The chat comes from the producing run (or its root run), else from
// the conversationId an app keeps in its metadata. The space is the chat's
// space at creation. A run or chat that no longer exists leaves the field
// empty; it does not fail the registration.
func (p ArtifactPlacement) Place(ctx context.Context, artifact *Artifact) error {
	if artifact == nil {
		return nil
	}
	provenance := &artifact.Provenance
	if provenance.ConversationID == "" && provenance.RunID != "" && p.Runs != nil {
		conversationID, err := p.runConversationID(ctx, artifact.Scope, provenance.RunID)
		if err != nil {
			return err
		}
		provenance.ConversationID = conversationID
	}
	if provenance.ConversationID == "" {
		if conversationID, _ := artifact.Metadata["conversationId"].(string); validOpaqueIdentifier(strings.TrimSpace(conversationID), 128) {
			provenance.ConversationID = strings.TrimSpace(conversationID)
		}
	}
	if provenance.SpaceID != "" || provenance.ConversationID == "" || p.Conversations == nil {
		return nil
	}
	conversation, err := p.Conversations.GetConversation(ctx, artifact.Scope, provenance.ConversationID)
	if errors.Is(err, ErrConversationNotFound) || (err == nil && conversation == nil) {
		return nil
	}
	if err != nil {
		return err
	}
	provenance.SpaceID = conversation.SpaceID
	return nil
}

func (p ArtifactPlacement) runConversationID(ctx context.Context, scope Scope, runID string) (string, error) {
	run, err := p.Runs.GetAgentRun(ctx, scope, runID)
	if errors.Is(err, ErrRunNotFound) || (err == nil && run == nil) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if id := runContextConversationID(run); id != "" {
		return id, nil
	}
	if run.RootRunID == "" || run.RootRunID == run.ID {
		return "", nil
	}
	root, err := p.Runs.GetAgentRun(ctx, scope, run.RootRunID)
	if errors.Is(err, ErrRunNotFound) || (err == nil && root == nil) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return runContextConversationID(root), nil
}

func runContextConversationID(run *AgentRun) string {
	id, _ := run.Context[conversationRunContextConversationID].(string)
	id = strings.TrimSpace(id)
	if !validOpaqueIdentifier(id, 128) {
		return ""
	}
	return id
}
