package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
)

// Worker-only pending projection. App reads still require an exact deployment
// and conversation, and all stores require a tenant scope.
func (s *MemoryStore) ListPendingSkillSetupRequests(_ context.Context, scope Scope, limit, offset int) ([]*SkillSetupRequest, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidSkillSetup
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*SkillSetupRequest
	for _, request := range s.skillSetupRequests {
		if request.Scope == scope && request.Status == "pending" {
			result = append(result, cloneSkillSetupRequest(request))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	limit, offset = normalizeExternalConversationPage(limit, offset)
	if offset >= len(result) {
		return []*SkillSetupRequest{}, nil
	}
	end := offset + limit
	if end > len(result) {
		end = len(result)
	}
	return result[offset:end], nil
}
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
