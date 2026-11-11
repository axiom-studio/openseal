package runtime

import "context"

func (s *SQLiteStore) HasSkillRuntimeMaintenanceUsage(ctx context.Context, scope Scope, skillID string) (bool, error) {
	if err := validateMaintenanceIdentity(scope, skillID); err != nil {
		return false, err
	}
	return logicalSkillRuntimeUsage(ctx, s.db, scope, skillID, "action_calls", "agent_runs", false)
}
func (s *PostgresStore) HasSkillRuntimeMaintenanceUsage(ctx context.Context, scope Scope, skillID string) (bool, error) {
	if err := validateMaintenanceIdentity(scope, skillID); err != nil {
		return false, err
	}
	return logicalSkillRuntimeUsage(ctx, s.db, scope, skillID, s.table("action_calls"), s.table("agent_runs"), true)
}
