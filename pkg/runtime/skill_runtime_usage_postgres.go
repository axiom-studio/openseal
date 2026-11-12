package runtime

import (
	"context"
)

func (s *PostgresStore) HasSkillRuntimeUsage(ctx context.Context, filter SkillRuntimeUsageFilter) (bool, error) {
	return hasSkillRuntimeUsage(ctx, s.db, filter, s.table("action_calls"), s.table("agent_runs"), true)
}

func (s *PostgresStore) ListReferencedSkillRuntimeVersions(ctx context.Context, scope Scope) ([]SkillRuntimeReference, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, listReferencedSkillRuntimeVersionsQuery(s.table("skill_bindings"), s.table("action_calls"), s.table("agent_runs"), true), scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReferencedSkillRuntimeVersions(rows)
}
