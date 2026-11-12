package postgres

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *PostgresStore) ListPendingSkillSetupRequests(ctx context.Context, scope models.Scope, limit, offset int) ([]*models.SkillSetupRequest, error) {
	return sqlstore.ListPendingSkillSetupRequests(ctx, s.db, s.table("skill_setup_requests"), scope, limit, offset)
}
