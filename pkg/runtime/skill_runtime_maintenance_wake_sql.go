package runtime

import (
	"context"
	"encoding/json"
)

func listCompletedMaintenanceWaiters(ctx context.Context, q maintenanceSQLQuery, runs, gates string, scope Scope, limit int, postgres bool) ([]*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		return nil, ErrSkillRuntimeMaintenanceConflict
	}
	query := `SELECT r.payload FROM ` + runs + ` r JOIN ` + gates + ` m ON m.scope_kind=r.scope_kind AND m.scope_id=r.scope_id AND m.skill_id=json_extract(r.payload,'$.wakeCondition.reference')
		WHERE r.scope_kind=? AND r.scope_id=? AND r.status='waiting_for_dependency' AND json_extract(r.payload,'$.wakeCondition.type')='skill_runtime_maintenance' AND m.active=0 ORDER BY r.id LIMIT ?`
	if postgres {
		query = `SELECT r.payload FROM ` + runs + ` r JOIN ` + gates + ` m ON m.scope_kind=r.scope_kind AND m.scope_id=r.scope_id AND m.skill_id=r.payload->'wakeCondition'->>'reference'
		WHERE r.scope_kind=$1 AND r.scope_id=$2 AND r.status='waiting_for_dependency' AND r.payload->'wakeCondition'->>'type'='skill_runtime_maintenance' AND m.active=FALSE ORDER BY r.id LIMIT $3`
	}
	rows, err := q.QueryContext(ctx, query, scope.Kind, scope.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*AgentRun, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var run AgentRun
		if err := json.Unmarshal([]byte(payload), &run); err != nil {
			return nil, err
		}
		if run.Scope != scope || run.Status != AgentRunStatusWaitingForDependency || run.WakeCondition == nil || run.WakeCondition.Type != skillRuntimeMaintenanceWakeType {
			return nil, ErrSkillRuntimeMaintenanceConflict
		}
		result = append(result, &run)
	}
	return result, rows.Err()
}
