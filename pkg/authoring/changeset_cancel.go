package authoring

import (
	"context"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// CancelPreparedGeneration durably fences a pending provider response. Hosts
// authenticate and authorize Actor before calling this tenant-scoped boundary.
// Cancellation preserves the original prompt and history for an explicit retry.
func (s *ChangeSetService) CancelPreparedGeneration(ctx context.Context, scope capability.ScopeReference, id string, expectedRevision int64, actor ChangeSetActor) (*ChangeSet, error) {
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" {
		return nil, fmt.Errorf("cancellation actor is required")
	}
	current, err := s.store.GetChangeSet(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if actor.Type != "user" || current.Actor != actor {
		return nil, ErrDraftOwner
	}
	if current.Revision != expectedRevision {
		return nil, ErrChangeSetRevision
	}
	if current.Status != ChangeSetEvaluating || current.Generation == nil {
		return nil, fmt.Errorf("%w: generation is not pending", ErrChangeSetTransition)
	}
	next := cloneChangeSet(current)
	next.Status = ChangeSetFailed
	next.Generation.Attempt++
	next.Generation.FailureCode = "canceled"
	next.Generation.LastError = "Generation stopped at your request"
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	next.Generation.CompletedAt = &next.UpdatedAt
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{
		Revision: next.Revision, From: current.Status, To: next.Status,
		Reason: "candidate_generation_canceled", Actor: actor, At: next.UpdatedAt,
	})
	return s.store.UpdateChangeSet(ctx, next, expectedRevision)
}
