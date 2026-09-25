package authoring

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/capability"
)

var ErrDraftOwner = errors.New("only the draft creator may delete it")

// DiscardDraft removes a draft from active authoring without erasing its audit.
// CAS also prevents a late model response or concurrent apply from reviving it.
func (s *ChangeSetService) DiscardDraft(ctx context.Context, scope capability.ScopeReference, id string, revision int64, actor ChangeSetActor) (*ChangeSet, error) {
	current, err := s.store.GetChangeSet(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if actor.Type != "user" || actor.ID == "" || current.Actor != actor {
		return nil, ErrDraftOwner
	}
	if current.Revision != revision {
		return nil, ErrChangeSetRevision
	}
	if current.Status == ChangeSetApplied || current.Status == ChangeSetRejected {
		return nil, ErrChangeSetTransition
	}
	next := cloneChangeSet(current)
	next.Status = ChangeSetRejected
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if next.Generation != nil && current.Status == ChangeSetEvaluating {
		next.Generation.FailureCode = "canceled"
		next.Generation.LastError = "Draft deleted by its creator"
		next.Generation.CompletedAt = &next.UpdatedAt
	}
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: next.Status, Reason: "draft_deleted", Actor: actor, At: next.UpdatedAt})
	return s.store.UpdateChangeSet(ctx, next, revision)
}
