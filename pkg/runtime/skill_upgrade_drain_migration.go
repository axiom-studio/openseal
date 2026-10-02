package runtime

import (
	"context"
	"database/sql"
)

const skillUpgradeDrainMigrationVersion int64 = 52

// This one-time projection keeps the recurring drain checks independent of
// ActionCall payload size. Later canonical action INSERTs write these columns
// alongside the immutable payload; action status already has its own column.
func migrateSkillUpgradeDrainSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	columns, err := sqliteTableColumns(tx, "action_calls")
	if err != nil {
		return err
	}
	added := false
	for _, name := range []string{"deployment_id", "binding_id", "skill_id", "skill_version"} {
		if !columns[name] {
			if _, err := tx.Exec("ALTER TABLE action_calls ADD COLUMN " + name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			added = true
		}
	}
	if !columns["binding_revision"] {
		if _, err := tx.Exec("ALTER TABLE action_calls ADD COLUMN binding_revision INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		added = true
	}
	if added {
		if _, err := tx.Exec(`UPDATE action_calls SET
			deployment_id=COALESCE(json_extract(payload,'$.deploymentId'),''),
			binding_id=COALESCE(json_extract(payload,'$.bindingId'),''),
			binding_revision=COALESCE(json_extract(payload,'$.bindingRevision'),0),
			skill_id=COALESCE(json_extract(payload,'$.skillId'),''),
			skill_version=COALESCE(json_extract(payload,'$.skillVersion'),'')`); err != nil {
			return err
		}
	}
	for _, statement := range skillUpgradeDrainIndexStatements("action_calls", "agent_runs", false) {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	// During a rolling upgrade, earlier binaries still write only payload. A
	// conditional projection preserves drain safety for those writes without
	// re-reading payloads for current binaries that provide the metadata.
	for _, operation := range []struct{ name, event string }{{"insert", "INSERT"}, {"update", "UPDATE OF payload"}} {
		statement := `CREATE TRIGGER IF NOT EXISTS project_skill_action_runtime_` + operation.name + `
			AFTER ` + operation.event + ` ON action_calls
			WHEN NEW.skill_id='' OR NEW.skill_version='' OR NEW.deployment_id=''
			BEGIN
			SELECT CASE WHEN COALESCE(json_extract(NEW.payload,'$.bindingId'),'')<>''
			AND EXISTS (SELECT 1 FROM skill_bindings b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id
				AND b.deployment_id=json_extract(NEW.payload,'$.deploymentId') AND b.id=json_extract(NEW.payload,'$.bindingId'))
			AND NOT EXISTS (SELECT 1 FROM skill_bindings b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id
				AND b.deployment_id=json_extract(NEW.payload,'$.deploymentId') AND b.id=json_extract(NEW.payload,'$.bindingId')
				AND b.revision=json_extract(NEW.payload,'$.bindingRevision') AND b.skill_id=json_extract(NEW.payload,'$.skillId')
				AND b.skill_version=json_extract(NEW.payload,'$.skillVersion') AND COALESCE(json_extract(b.payload,'$.disabled'),0)=0)
			THEN RAISE(ABORT,'selected skill binding is unavailable or stale') END;
			UPDATE action_calls SET
			deployment_id=COALESCE(json_extract(NEW.payload,'$.deploymentId'),''),
			binding_id=COALESCE(json_extract(NEW.payload,'$.bindingId'),''),
			binding_revision=COALESCE(json_extract(NEW.payload,'$.bindingRevision'),0),
			skill_id=COALESCE(json_extract(NEW.payload,'$.skillId'),''),
			skill_version=COALESCE(json_extract(NEW.payload,'$.skillVersion'),'')
			WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND id=NEW.id; END`
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) migrateSkillUpgradeDrain(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, skillUpgradeDrainMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+s.table("action_calls")+`
		ADD COLUMN IF NOT EXISTS deployment_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS binding_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS binding_revision BIGINT NOT NULL DEFAULT 0,
		ADD COLUMN IF NOT EXISTS skill_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS skill_version TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE "+s.table("action_calls")+` SET
		deployment_id=COALESCE(payload->>'deploymentId',''),
		binding_id=COALESCE(payload->>'bindingId',''),
		binding_revision=COALESCE((payload->>'bindingRevision')::bigint,0),
		skill_id=COALESCE(payload->>'skillId',''),
		skill_version=COALESCE(payload->>'skillVersion','')`); err != nil {
		return err
	}
	for _, statement := range skillUpgradeDrainIndexStatements(s.table("action_calls"), s.table("agent_runs"), true) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE FUNCTION `+s.table("project_skill_action_runtime_metadata")+`() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE
			binding_version TEXT;
			binding_revision BIGINT;
			binding_skill_id TEXT;
			binding_disabled BOOLEAN;
		BEGIN
			NEW.deployment_id=COALESCE(NEW.payload->>'deploymentId','');
			NEW.binding_id=COALESCE(NEW.payload->>'bindingId','');
			NEW.binding_revision=COALESCE((NEW.payload->>'bindingRevision')::bigint,0);
			NEW.skill_id=COALESCE(NEW.payload->>'skillId','');
			NEW.skill_version=COALESCE(NEW.payload->>'skillVersion','');
			IF NEW.binding_id<>'' THEN
				SELECT skill_version,revision,skill_id,COALESCE((payload->>'disabled')::boolean,false)
				INTO binding_version,binding_revision,binding_skill_id,binding_disabled
				FROM `+s.table("skill_bindings")+` WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id
					AND deployment_id=NEW.deployment_id AND id=NEW.binding_id FOR SHARE;
				IF FOUND AND (binding_disabled OR binding_revision<>NEW.binding_revision
					OR binding_skill_id<>NEW.skill_id OR binding_version<>NEW.skill_version) THEN
					RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='selected skill binding is unavailable or stale';
				END IF;
			END IF;
			RETURN NEW;
		END $$;
		DROP TRIGGER IF EXISTS project_skill_action_runtime_metadata ON `+s.table("action_calls")+`;
		CREATE TRIGGER project_skill_action_runtime_metadata BEFORE INSERT OR UPDATE OF payload ON `+s.table("action_calls")+`
		FOR EACH ROW WHEN (NEW.skill_id='' OR NEW.skill_version='' OR NEW.deployment_id='')
		EXECUTE FUNCTION `+s.table("project_skill_action_runtime_metadata")+`()`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO "+s.table("schema_migrations")+" (version,name) VALUES ($1,'indexed Skill runtime usage and upgrade drain')", skillUpgradeDrainMigrationVersion)
	return err
}

func skillUpgradeDrainIndexStatements(actions, runs string, postgres bool) []string {
	prefix := "idx_"
	if postgres {
		prefix = ""
	}
	return []string{
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_runtime_active ON " + actions + " (scope_kind,scope_id,skill_id,skill_version,status) WHERE status IN (" + unfinishedSkillActionSQL + ")",
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_binding_active ON " + actions + " (scope_kind,scope_id,deployment_id,binding_id,skill_id,skill_version,status) WHERE status IN (" + unfinishedSkillActionSQL + ")",
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_run_receipt ON " + actions + " (scope_kind,scope_id,run_id,skill_id,skill_version,deployment_id,binding_id) WHERE status='succeeded'",
		"CREATE INDEX IF NOT EXISTS " + prefix + "agent_runs_skill_runtime_active ON " + runs + " (scope_kind,scope_id,status,id) WHERE status IN (" + unfinishedSkillRunSQL + ")",
	}
}
