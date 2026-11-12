package runtime

import (
	"context"
	"database/sql"
	"fmt"
)

func skillRuntimeMaintenanceLockKey(scope Scope, skillID string) string {
	return fmt.Sprintf("skill-runtime-maintenance:%d:%s:%d:%s:%d:%s", len(scope.Kind), scope.Kind, len(scope.ID), scope.ID, len(skillID), skillID)
}

func logicalSkillRuntimeUsage(ctx context.Context, q skillRuntimeUsageQuerier, scope Scope, skillID, actions, runs string, postgres bool) (bool, error) {
	parameter := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	identity := "a.scope_kind=" + parameter(1) + " AND a.scope_id=" + parameter(2) + " AND a.skill_id=" + parameter(3)
	query := "SELECT EXISTS (SELECT 1 FROM " + actions + " a WHERE " + identity + " AND a.status IN (" + unfinishedSkillActionSQL + ") LIMIT 1) OR EXISTS (SELECT 1 FROM " + runs + " r WHERE r.scope_kind=" + parameter(1) + " AND r.scope_id=" + parameter(2) + " AND r.status IN (" + unfinishedSkillRunSQL + ") AND EXISTS (SELECT 1 FROM " + actions + " a WHERE " + identity + " AND a.run_id=r.id AND a.status='succeeded' LIMIT 1) LIMIT 1)"
	args := []interface{}{scope.Kind, scope.ID, skillID}
	if !postgres {
		args = append(args, scope.Kind, scope.ID, scope.Kind, scope.ID, skillID)
	}
	query += " OR EXISTS (SELECT 1 FROM " + skillRuntimeDependencyTable(runs) + " d WHERE d.scope_kind=" + parameter(1) + " AND d.scope_id=" + parameter(2) + " AND d.skill_id=" + parameter(3) + " LIMIT 1)"
	if !postgres {
		args = append(args, scope.Kind, scope.ID, skillID)
	}
	var busy bool
	err := q.QueryRowContext(ctx, query, args...).Scan(&busy)
	return busy, err
}

func maintenanceBindingsMatch(ctx context.Context, q skillRuntimeUsageQuerier, g *SkillRuntimeMaintenance, version, bindings string, postgres bool) (bool, error) {
	parameter := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	enabled := "COALESCE(json_extract(payload,'$.disabled'),0)=0"
	if postgres {
		enabled = "COALESCE((payload->>'disabled')::boolean,false)=false"
	}
	query := "SELECT NOT EXISTS (SELECT 1 FROM " + bindings + " WHERE scope_kind=" + parameter(1) + " AND scope_id=" + parameter(2) + " AND skill_id=" + parameter(3) + " AND " + enabled + " AND (skill_version<>" + parameter(4) + " OR source_identity<>" + parameter(5) + ") LIMIT 1)"
	var matched bool
	err := q.QueryRowContext(ctx, query, g.Scope.Kind, g.Scope.ID, g.SkillID, version, g.SourceIdentity).Scan(&matched)
	return matched, err
}

var _ skillRuntimeUsageQuerier = (*sql.Conn)(nil)
