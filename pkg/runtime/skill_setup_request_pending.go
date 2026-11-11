package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
)

func (s *PostgresStore) ListPendingSkillSetupRequests(ctx context.Context, scope Scope, limit, offset int) ([]*SkillSetupRequest, error) {
	return listPendingSkillSetupRequests(ctx, s.db, s.table("skill_setup_requests"), scope, limit, offset)
}
func (s *SQLiteStore) ListPendingSkillSetupRequests(ctx context.Context, scope Scope, limit, offset int) ([]*SkillSetupRequest, error) {
	return listPendingSkillSetupRequests(ctx, s.db, "skill_setup_requests", scope, limit, offset)
}
func listPendingSkillSetupRequests(ctx context.Context, db *sql.DB, table string, scope Scope, limit, offset int) ([]*SkillSetupRequest, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidSkillSetup
	}
	limit, offset = normalizeExternalConversationPage(limit, offset)
	rows, err := db.QueryContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind=$1 AND scope_id=$2 AND status='pending' ORDER BY created_at,id LIMIT $3 OFFSET $4`, scope.Kind, scope.ID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*SkillSetupRequest{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var request SkillSetupRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, err
		}
		result = append(result, &request)
	}
	return result, rows.Err()
}
