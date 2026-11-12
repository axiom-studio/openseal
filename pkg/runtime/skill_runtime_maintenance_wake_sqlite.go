package runtime

import (
	"context"
)

func (s *SQLiteStore) ListCompletedSkillRuntimeMaintenanceWaiters(ctx context.Context, scope Scope, limit int) ([]*AgentRun, error) {
	return listCompletedMaintenanceWaiters(ctx, s.db, "agent_runs", "skill_runtime_maintenance", scope, limit, false)
}
