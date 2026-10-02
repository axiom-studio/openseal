package runtime

import "context"

// HasSkillRuntimeMaintenanceUsage rechecks logical Skill work using indexed
// metadata, including active receipts. It does not read action output payloads.
func (s *MemoryStore) HasSkillRuntimeMaintenanceUsage(_ context.Context, scope Scope, skillID string) (bool, error) {
	if err := validateMaintenanceIdentity(scope, skillID); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.skillRuntimeUsage[memorySkillRuntimeUsageKey{Scope: scope, SkillID: skillID}] > 0, nil
}
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
