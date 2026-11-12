package runtime

import (
	"context"
)

func (s *PostgresStore) ListCompletedSkillRuntimeMaintenanceWaiters(ctx context.Context, scope Scope, limit int) ([]*AgentRun, error) {
	return listCompletedMaintenanceWaiters(ctx, s.db, s.table("agent_runs"), s.table("skill_runtime_maintenance"), scope, limit, true)
}
