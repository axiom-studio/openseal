package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *PostgresStore) ListCompletedSkillRuntimeMaintenanceWaiters(ctx context.Context, scope models.Scope, limit int) ([]*models.AgentRun, error) {
	return sqlstore.ListCompletedMaintenanceWaiters(ctx, s.db, s.table("agent_runs"), s.table("skill_runtime_maintenance"), scope, limit, true)
}
