package runtime

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/authoring"
)

func (s *PostgresStore) ListDraftChangeSets(ctx context.Context, q authoring.DraftChangeSetQuery) ([]*authoring.ChangeSet, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.payload FROM `+s.table("workforce_change_sets")+` d WHERE d.scope_kind=$1 AND d.scope_id=$2 AND d.payload->'actor'->>'type'=$3 AND d.payload->'actor'->>'id'=$4 AND d.status NOT IN ('applied','rejected') AND NOT EXISTS (SELECT 1 FROM `+s.table("workforce_change_sets")+` c WHERE c.scope_kind=d.scope_kind AND c.scope_id=d.scope_id AND c.parent_id=d.id AND c.payload->'actor'->>'type'=$3 AND c.payload->'actor'->>'id'=$4) ORDER BY d.updated_at DESC, d.id LIMIT $5 OFFSET $6`, q.Scope.Kind, q.Scope.ID, q.Actor.Type, q.Actor.ID, q.Limit, q.Offset)
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
