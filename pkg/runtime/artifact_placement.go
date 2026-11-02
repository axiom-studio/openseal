package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ArtifactRunReader and ArtifactConversationReader are the lookups an
// ArtifactPlacement needs. Kernel stores and services both satisfy them.
type ArtifactRunReader interface {
	GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error)
}

type ArtifactConversationReader interface {
	GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error)
}

// ArtifactSpaceResolver places an artifact made outside any chat the way a
// conversation created without a space is placed: the owner's first space by
// position, else the scope's Personal space, created when needed.
type ArtifactSpaceResolver interface {
	OwnerSpaceID(ctx context.Context, scope Scope, owner *ObjectiveOwner, now time.Time) (string, error)
}

// ArtifactPlacement records where an artifact was made: the chat whose run
// produced it and the space that chat lives in, else the owner's space. Any
// reader may be nil; placement then fills only what it can.
type ArtifactPlacement struct {
	Runs          ArtifactRunReader
	Conversations ArtifactConversationReader
	Spaces        ArtifactSpaceResolver
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
	if spaces, ok := store.(ArtifactSpaceResolver); ok {
		placement.Spaces = spaces
	}
	return placement
}

// Place fills Provenance.ConversationID and Provenance.SpaceID when they are
// empty. The chat comes from the producing run (or its root run), else from
// the conversationId an app keeps in its metadata. The space is the chat's
// space at creation; an artifact made outside any chat, or whose chat no
// longer exists, takes its owner's space, else Personal. A run or chat that
// no longer exists does not fail the registration.
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
	if provenance.SpaceID == "" && provenance.ConversationID != "" && p.Conversations != nil {
		conversation, err := p.Conversations.GetConversation(ctx, artifact.Scope, provenance.ConversationID)
		if err != nil && !errors.Is(err, ErrConversationNotFound) {
			return err
		}
		if err == nil && conversation != nil {
			provenance.SpaceID = conversation.SpaceID
		}
	}
	if provenance.SpaceID != "" || p.Spaces == nil {
		return nil
	}
	now := artifact.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	spaceID, err := p.Spaces.OwnerSpaceID(ctx, artifact.Scope, provenance.Owner, now)
	if err != nil {
		return err
	}
	provenance.SpaceID = spaceID
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
