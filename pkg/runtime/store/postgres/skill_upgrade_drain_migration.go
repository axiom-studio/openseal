package postgres

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

const skillUpgradeDrainMigrationVersion int64 = 52

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
	for _, statement := range sqlstore.SkillUpgradeDrainIndexStatements(s.table("action_calls"), s.table("agent_runs"), true) {
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
