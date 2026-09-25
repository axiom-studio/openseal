package runtime

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/authoring"
)

func (s *SQLiteStore) ListDraftChangeSets(ctx context.Context, q authoring.DraftChangeSetQuery) ([]*authoring.ChangeSet, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM workforce_change_sets WHERE scope_kind=? AND scope_id=? AND json_extract(payload, '$.actor.type')=? AND json_extract(payload, '$.actor.id')=? AND status NOT IN ('applied','rejected') ORDER BY updated_at DESC, id LIMIT ? OFFSET ?`, q.Scope.Kind, q.Scope.ID, q.Actor.Type, q.Actor.ID, q.Limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*authoring.ChangeSet, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		value, err := decodeChangeSet(payload)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
