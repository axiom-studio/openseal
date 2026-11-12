package sqlite

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *Store) HasSkillRuntimeUsage(ctx context.Context, filter models.SkillRuntimeUsageFilter) (bool, error) {
	return sqlstore.HasSkillRuntimeUsage(ctx, s.db, filter, "action_calls", "agent_runs", false)
}

func (s *Store) ListReferencedSkillRuntimeVersions(ctx context.Context, scope models.Scope) ([]models.SkillRuntimeReference, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlstore.ListReferencedSkillRuntimeVersionsQuery("skill_bindings", "action_calls", "agent_runs", false), scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return sqlstore.ScanReferencedSkillRuntimeVersions(rows)
}
