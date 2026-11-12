package runtime

import (
	"database/sql"
)

func migrateSkillRuntimeMaintenanceSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS skill_runtime_maintenance(scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,skill_id TEXT NOT NULL,active INTEGER NOT NULL,revision INTEGER NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,skill_id))`,
		`CREATE INDEX IF NOT EXISTS skill_runtime_maintenance_active ON skill_runtime_maintenance(scope_kind,scope_id,skill_id) WHERE active=1`,
		`CREATE INDEX IF NOT EXISTS skill_bindings_maintenance_page ON skill_bindings(scope_kind,scope_id,skill_id,deployment_id,id)`,
		`CREATE INDEX IF NOT EXISTS agent_runs_skill_maintenance_wait ON agent_runs(scope_kind,scope_id,json_extract(payload,'$.wakeCondition.reference'),id) WHERE status='waiting_for_dependency' AND json_extract(payload,'$.wakeCondition.type')='skill_runtime_maintenance'`,
		`CREATE TRIGGER IF NOT EXISTS fence_skill_runtime_maintenance BEFORE INSERT ON action_calls
		WHEN EXISTS(SELECT 1 FROM skill_runtime_maintenance m WHERE m.scope_kind=NEW.scope_kind AND m.scope_id=NEW.scope_id
			AND m.skill_id=COALESCE(NULLIF(NEW.skill_id,''),json_extract(NEW.payload,'$.skillId')) AND m.active=1)
		BEGIN SELECT RAISE(ABORT,'Skill runtime maintenance is in progress'); END`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
