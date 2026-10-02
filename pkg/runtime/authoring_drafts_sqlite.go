package runtime

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/authoring"
)

func (s *SQLiteStore) ListDraftChangeSets(ctx context.Context, q authoring.DraftChangeSetQuery) ([]*authoring.ChangeSet, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.payload FROM workforce_change_sets d WHERE d.scope_kind=? AND d.scope_id=? AND json_extract(d.payload, '$.actor.type')=? AND json_extract(d.payload, '$.actor.id')=? AND d.status NOT IN ('applied','rejected') AND NOT EXISTS (SELECT 1 FROM workforce_change_sets c WHERE c.scope_kind=d.scope_kind AND c.scope_id=d.scope_id AND c.parent_id=d.id AND json_extract(c.payload, '$.actor.type')=json_extract(d.payload, '$.actor.type') AND json_extract(c.payload, '$.actor.id')=json_extract(d.payload, '$.actor.id')) ORDER BY d.updated_at DESC, d.id LIMIT ? OFFSET ?`, q.Scope.Kind, q.Scope.ID, q.Actor.Type, q.Actor.ID, q.Limit, q.Offset)
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
