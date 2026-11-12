package sqlite

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *SQLiteStore) ListPendingSkillSetupRequests(ctx context.Context, scope models.Scope, limit, offset int) ([]*models.SkillSetupRequest, error) {
	return sqlstore.ListPendingSkillSetupRequests(ctx, s.db, "skill_setup_requests", scope, limit, offset)
}
