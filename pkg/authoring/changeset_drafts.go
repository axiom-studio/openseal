package authoring

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// DraftChangeSetQuery is explicitly owner- and tenant-scoped. Hosts supply the
// authenticated actor, never an arbitrary actor selected by the browser.
type DraftChangeSetQuery struct {
	Scope  capability.ScopeReference
	Actor  ChangeSetActor
	Limit  int
	Offset int
}

func (q DraftChangeSetQuery) Validate() error {
	if strings.TrimSpace(q.Scope.Kind) == "" || strings.TrimSpace(q.Scope.ID) == "" || strings.TrimSpace(q.Actor.Type) == "" || strings.TrimSpace(q.Actor.ID) == "" || q.Limit < 1 || q.Limit > 100 || q.Offset < 0 {
		return errors.New("draft scope, actor, limit (1–100), and nonnegative offset are required")
	}
	return nil
}

type DraftChangeSetStore interface {
	ListDraftChangeSets(context.Context, DraftChangeSetQuery) ([]*ChangeSet, error)
}

func (s *ChangeSetService) ListDrafts(ctx context.Context, q DraftChangeSetQuery) ([]*ChangeSet, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	store, ok := s.store.(DraftChangeSetStore)
	if !ok {
		return nil, errors.New("draft listing is unavailable")
	}
	return store.ListDraftChangeSets(ctx, q)
}

func (s *MemoryChangeSetStore) ListDraftChangeSets(_ context.Context, q DraftChangeSetQuery) ([]*ChangeSet, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]*ChangeSet, 0)
	for _, value := range s.changeSets {
		if value.Scope == q.Scope && value.Actor == q.Actor && value.Status != ChangeSetApplied && value.Status != ChangeSetRejected {
			values = append(values, cloneChangeSet(value))
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	if q.Offset >= len(values) {
		return []*ChangeSet{}, nil
	}
	values = values[q.Offset:]
	if len(values) > q.Limit {
		values = values[:q.Limit]
	}
	return values, nil
}
