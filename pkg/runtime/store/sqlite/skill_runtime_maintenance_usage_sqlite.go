package sqlite

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *SQLiteStore) HasSkillRuntimeMaintenanceUsage(ctx context.Context, scope models.Scope, skillID string) (bool, error) {
	if err := models.ValidateMaintenanceIdentity(scope, skillID); err != nil {
		return false, err
	}
	return sqlstore.LogicalSkillRuntimeUsage(ctx, s.db, scope, skillID, "action_calls", "agent_runs", false)
}
