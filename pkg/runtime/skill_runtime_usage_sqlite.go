package runtime

import (
	"context"
)

func (s *SQLiteStore) HasSkillRuntimeUsage(ctx context.Context, filter SkillRuntimeUsageFilter) (bool, error) {
	return hasSkillRuntimeUsage(ctx, s.db, filter, "action_calls", "agent_runs", false)
}

func (s *SQLiteStore) ListReferencedSkillRuntimeVersions(ctx context.Context, scope Scope) ([]SkillRuntimeReference, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, listReferencedSkillRuntimeVersionsQuery("skill_bindings", "action_calls", "agent_runs", false), scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReferencedSkillRuntimeVersions(rows)
}
