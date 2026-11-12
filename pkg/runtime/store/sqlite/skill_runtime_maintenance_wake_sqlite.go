package sqlite

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *Store) ListCompletedSkillRuntimeMaintenanceWaiters(ctx context.Context, scope models.Scope, limit int) ([]*models.AgentRun, error) {
	return sqlstore.ListCompletedMaintenanceWaiters(ctx, s.db, "agent_runs", "skill_runtime_maintenance", scope, limit, false)
}
