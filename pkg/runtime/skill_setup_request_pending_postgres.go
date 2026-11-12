package runtime

import (
	"context"
)

func (s *PostgresStore) ListPendingSkillSetupRequests(ctx context.Context, scope Scope, limit, offset int) ([]*SkillSetupRequest, error) {
	return listPendingSkillSetupRequests(ctx, s.db, s.table("skill_setup_requests"), scope, limit, offset)
}
