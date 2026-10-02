package runtime

import (
	"context"
	"database/sql"
)

const skillRuntimeMaintenanceMigrationVersion int64 = 53

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

func (s *PostgresStore) migrateSkillRuntimeMaintenance(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, skillRuntimeMaintenanceMigrationVersion)
	if err != nil || applied {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + s.table("skill_runtime_maintenance") + `(scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,skill_id TEXT NOT NULL,active BOOLEAN NOT NULL,revision BIGINT NOT NULL,payload JSONB NOT NULL,PRIMARY KEY(scope_kind,scope_id,skill_id))`,
		`CREATE INDEX IF NOT EXISTS skill_runtime_maintenance_active ON ` + s.table("skill_runtime_maintenance") + `(scope_kind,scope_id,skill_id) WHERE active=TRUE`,
		`CREATE INDEX IF NOT EXISTS skill_bindings_maintenance_page ON ` + s.table("skill_bindings") + `(scope_kind,scope_id,skill_id,deployment_id,id)`,
		`CREATE INDEX IF NOT EXISTS agent_runs_skill_maintenance_wait ON ` + s.table("agent_runs") + `(scope_kind,scope_id,((payload->'wakeCondition')->>'reference'),id) WHERE status='waiting_for_dependency' AND ((payload->'wakeCondition')->>'type')='skill_runtime_maintenance'`,
		`CREATE OR REPLACE FUNCTION ` + s.table("fence_skill_runtime_maintenance") + `() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE logical_skill TEXT; lock_key TEXT;
		BEGIN
		logical_skill=COALESCE(NULLIF(NEW.skill_id,''),NEW.payload->>'skillId','');
		lock_key='skill-runtime-maintenance:'||octet_length(NEW.scope_kind)::text||':'||NEW.scope_kind||':'||octet_length(NEW.scope_id)::text||':'||NEW.scope_id||':'||octet_length(logical_skill)::text||':'||logical_skill;
		PERFORM pg_advisory_xact_lock_shared(hashtextextended(lock_key,0));
		IF EXISTS(SELECT 1 FROM ` + s.table("skill_runtime_maintenance") + ` m WHERE m.scope_kind=NEW.scope_kind AND m.scope_id=NEW.scope_id AND m.skill_id=logical_skill AND m.active) THEN
		RAISE EXCEPTION USING ERRCODE='55P03',MESSAGE='Skill runtime maintenance is in progress'; END IF;
		RETURN NEW;
		END $$`,
		`DROP TRIGGER IF EXISTS fence_skill_runtime_maintenance ON ` + s.table("action_calls") + `; CREATE TRIGGER fence_skill_runtime_maintenance BEFORE INSERT ON ` + s.table("action_calls") + ` FOR EACH ROW EXECUTE FUNCTION ` + s.table("fence_skill_runtime_maintenance") + `()`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'durable tenant Skill runtime maintenance')`, skillRuntimeMaintenanceMigrationVersion)
	return err
}
