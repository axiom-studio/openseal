package runtime

import (
	"database/sql"
)

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
