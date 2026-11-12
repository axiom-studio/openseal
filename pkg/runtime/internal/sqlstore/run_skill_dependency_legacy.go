package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
)

const LegacyRunSkillDependencyBatch = 1000

// Legacy payloads are inspected once, during migration, in bounded keyset
// pages. An exact persisted method identity must prove the current immutable
// implementation. Unknown historical methods are never inferred from a newer
// active definition, and the historical Run payload is never rewritten.
func BackfillLegacyRunSkillDependencies(ctx context.Context, tx *sql.Tx, runs, deployments, definitions, dependencies string, postgres bool) error {
	cursor := [3]string{}
	for {
		where := ""
		var args []any
		if cursor[0] != "" {
			where = ` AND (scope_kind,scope_id,id)>(?,?,?)`
			if postgres {
				where = ` AND (scope_kind,scope_id,id)>($1,$2,$3)`
			}
			args = []any{cursor[0], cursor[1], cursor[2]}
		}
		query := `SELECT r.scope_kind,r.scope_id,r.id,r.assigned_agent_id,r.payload,COALESCE(d.payload,` + legacyEmptyJSON(postgres) + `),COALESCE(f.payload,` + legacyEmptyJSON(postgres) + `)
 FROM (SELECT scope_kind,scope_id,id,assigned_agent_id,payload FROM ` + runs + ` WHERE status IN (` + UnfinishedSkillRunSQL + `)` + where + ` ORDER BY scope_kind,scope_id,id LIMIT 1000) r
 LEFT JOIN ` + deployments + ` d ON d.scope_kind=r.scope_kind AND d.scope_id=r.scope_id AND d.id=r.assigned_agent_id
 LEFT JOIN ` + definitions + ` f ON f.id=d.definition_id AND f.version=d.active_version ORDER BY r.scope_kind,r.scope_id,r.id`
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		type item struct {
			scope                                     models.Scope
			id, assigned, run, deployment, definition string
		}
		page := make([]item, 0, LegacyRunSkillDependencyBatch)
		for rows.Next() {
			var value item
			if err := rows.Scan(&value.scope.Kind, &value.scope.ID, &value.id, &value.assigned, &value.run, &value.deployment, &value.definition); err != nil {
				rows.Close()
				return err
			}
			page = append(page, value)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, value := range page {
			cursor = [3]string{value.scope.Kind, value.scope.ID, value.id}
			var run models.AgentRun
			var deployment kernelagent.AgentDeployment
			var definition kernelagent.AgentDefinition
			if json.Unmarshal([]byte(value.run), &run) != nil || json.Unmarshal([]byte(value.deployment), &deployment) != nil || json.Unmarshal([]byte(value.definition), &definition) != nil || run.Scope != value.scope || run.ID != value.id || run.AssignedAgentID != value.assigned {
				continue
			}
			pin := models.LegacyAcceptedRunExecution(&run, &deployment, &definition)
			if pin == nil {
				continue
			}
			insert := `INSERT OR IGNORE INTO ` + dependencies + ` VALUES(?,?,?,?,?,?)`
			if postgres {
				insert = `INSERT INTO ` + dependencies + ` VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`
			}
			for _, ref := range pin.SkillDependencies {
				if _, err := tx.ExecContext(ctx, insert, run.Scope.Kind, run.Scope.ID, run.ID, ref.SkillID, ref.SkillVersion, models.RunSkillDependencyDeployment(&run)); err != nil {
					return err
				}
			}
		}
		if len(page) < LegacyRunSkillDependencyBatch {
			return nil
		}
	}
}

func legacyEmptyJSON(postgres bool) string {
	if postgres {
		return `'{}'::jsonb`
	}
	return `'{}'`
}
