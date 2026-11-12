package runtime

import (
	"context"
)

func (s *SQLiteStore) ListPendingSkillSetupRequests(ctx context.Context, scope Scope, limit, offset int) ([]*SkillSetupRequest, error) {
	return listPendingSkillSetupRequests(ctx, s.db, "skill_setup_requests", scope, limit, offset)
}
