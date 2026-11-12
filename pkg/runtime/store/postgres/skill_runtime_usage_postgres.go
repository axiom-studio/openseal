package postgres

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *PostgresStore) HasSkillRuntimeUsage(ctx context.Context, filter models.SkillRuntimeUsageFilter) (bool, error) {
	return sqlstore.HasSkillRuntimeUsage(ctx, s.db, filter, s.table("action_calls"), s.table("agent_runs"), true)
}

func (s *PostgresStore) ListReferencedSkillRuntimeVersions(ctx context.Context, scope models.Scope) ([]models.SkillRuntimeReference, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlstore.ListReferencedSkillRuntimeVersionsQuery(s.table("skill_bindings"), s.table("action_calls"), s.table("agent_runs"), true), scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return sqlstore.ScanReferencedSkillRuntimeVersions(rows)
}
