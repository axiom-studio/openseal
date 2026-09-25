package openseal

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/authoring"
)

type WorkforceDraftQuery = authoring.DraftChangeSetQuery

var ErrWorkforceDraftOwner = authoring.ErrDraftOwner

func (e *Engine) CancelWorkforceDraft(ctx context.Context, scope SkillScope, id string, revision int64, actor WorkforceChangeSetActor) (*authoring.ChangeSet, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, errors.New("workforce change sets are unavailable")
	}
	return e.authoringChanges.CancelPreparedGeneration(ctx, scope, id, revision, actor)
}

func (e *Engine) DiscardWorkforceDraft(ctx context.Context, scope SkillScope, id string, revision int64, actor WorkforceChangeSetActor) (*authoring.ChangeSet, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, errors.New("workforce change sets are unavailable")
	}
	return e.authoringChanges.DiscardDraft(ctx, scope, id, revision, actor)
}

func (e *Engine) ListWorkforceDrafts(ctx context.Context, q WorkforceDraftQuery) ([]*authoring.ChangeSet, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, errors.New("workforce change sets are unavailable")
	}
	return e.authoringChanges.ListDrafts(ctx, q)
}
