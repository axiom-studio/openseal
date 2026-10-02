package runtime

import (
	"context"
	"database/sql"
	"strings"
)

const projectSkillReferenceMigrationVersion int64 = 55

func sqliteProjectSkillMonitorPayload(alias string) string {
	return `(CASE WHEN json_valid(` + alias + `.payload) THEN ` + alias + `.payload ELSE '{}' END)`
}

func sqliteProjectSkillMonitors(alias string) string {
	payload := sqliteProjectSkillMonitorPayload(alias)
	return `json_each(CASE WHEN json_type(` + payload + `,'$.sourceMonitors')='array' THEN json_extract(` + payload + `,'$.sourceMonitors') ELSE '[]' END)`
}

func sqliteProjectSkillMonitorValid(alias string) string {
	value := `(CASE WHEN ` + alias + `.type='object' THEN ` + alias + `.value ELSE '{}' END)`
	checks := []string{alias + `.type='object'`}
	for _, field := range []string{"id", "assignedAgentId", "skillId", "skillVersion", "action"} {
		checks = append(checks, `json_type(`+value+`,'$.`+field+`')='text'`, `length(trim(json_extract(`+value+`,'$.`+field+`')))>0`, `length(json_extract(`+value+`,'$.`+field+`'))<=128`)
	}
	return "COALESCE((" + strings.Join(checks, " AND ") + "),0)"
}

func sqliteProjectSkillMonitorChanged() string {
	// JSON tree comparison treats harmless object member ordering as unchanged.
	return `NEW.owner_id<>OLD.owner_id OR NEW.scope_kind<>OLD.scope_kind OR NEW.scope_id<>OLD.scope_id OR NOT EXISTS(SELECT 1 FROM ` + sqliteProjectSkillMonitors("OLD") + ` old WHERE json_extract(CASE WHEN old.type='object' THEN old.value ELSE '{}' END,'$.id')=json_extract(ref.value,'$.id') AND NOT EXISTS(SELECT fullkey,type,atom FROM json_tree(ref.value) EXCEPT SELECT fullkey,type,atom FROM json_tree(CASE WHEN old.type='object' THEN old.value ELSE '{}' END)) AND NOT EXISTS(SELECT fullkey,type,atom FROM json_tree(CASE WHEN old.type='object' THEN old.value ELSE '{}' END) EXCEPT SELECT fullkey,type,atom FROM json_tree(ref.value)))`
}

func migrateProjectSkillReferencesSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='project_skill_references')`).Scan(&exists); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS project_skill_references(scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,project_id TEXT NOT NULL,monitor_id TEXT NOT NULL,project_revision INTEGER NOT NULL,owner_id TEXT NOT NULL,assigned_agent_id TEXT NOT NULL,skill_id TEXT NOT NULL,skill_version TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,project_id,monitor_id))`,
		`CREATE INDEX IF NOT EXISTS idx_project_skill_references_assigned ON project_skill_references(scope_kind,scope_id,skill_id,skill_version,assigned_agent_id,project_id,monitor_id,project_revision)`,
		`CREATE INDEX IF NOT EXISTS idx_project_skill_references_owner ON project_skill_references(scope_kind,scope_id,skill_id,skill_version,owner_id,project_id,monitor_id,project_revision)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	projection := func(alias string) string {
		return `SELECT ` + alias + `.scope_kind,` + alias + `.scope_id,` + alias + `.id,json_extract(ref.value,'$.id'),` + alias + `.revision,` + alias + `.owner_id,json_extract(ref.value,'$.assignedAgentId'),json_extract(ref.value,'$.skillId'),json_extract(ref.value,'$.skillVersion') FROM ` + sqliteProjectSkillMonitors(alias) + ` ref WHERE ` + sqliteProjectSkillMonitorValid("ref")
	}
	if !exists {
		// Only this initial backfill reads historical Project JSON. Reconcile and
		// final cutover use indexed identity and revision metadata thereafter.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO project_skill_references SELECT p.scope_kind,p.scope_id,p.id,json_extract(ref.value,'$.id'),p.revision,p.owner_id,json_extract(ref.value,'$.assignedAgentId'),json_extract(ref.value,'$.skillId'),json_extract(ref.value,'$.skillVersion') FROM projects p,` + sqliteProjectSkillMonitors("p") + ` ref WHERE ` + sqliteProjectSkillMonitorValid("ref")); err != nil {
			return err
		}
	}
	for _, operation := range []struct{ name, event string }{{"insert", "INSERT"}, {"update", "UPDATE"}} {
		changed := "1"
		if operation.name == "update" {
			changed = "(" + sqliteProjectSkillMonitorChanged() + ")"
		}
		statement := `CREATE TRIGGER IF NOT EXISTS admit_project_skill_references_` + operation.name + ` BEFORE ` + operation.event + ` ON projects BEGIN
		SELECT CASE WHEN json_type(` + sqliteProjectSkillMonitorPayload("NEW") + `,'$.sourceMonitors') IS NOT NULL AND json_type(` + sqliteProjectSkillMonitorPayload("NEW") + `,'$.sourceMonitors') NOT IN ('array','null') OR EXISTS(SELECT 1 FROM ` + sqliteProjectSkillMonitors("NEW") + ` ref WHERE NOT (` + sqliteProjectSkillMonitorValid("ref") + `)) OR EXISTS(SELECT 1 FROM ` + sqliteProjectSkillMonitors("NEW") + ` ref GROUP BY json_extract(CASE WHEN ref.type='object' THEN ref.value ELSE '{}' END,'$.id') HAVING count(*)>1) THEN RAISE(ABORT,'Project source monitor identity is invalid') END;
		SELECT CASE WHEN EXISTS(SELECT 1 FROM ` + sqliteProjectSkillMonitors("NEW") + ` ref WHERE ` + changed + ` AND EXISTS(SELECT 1 FROM skill_bindings b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.deployment_id=json_extract(ref.value,'$.assignedAgentId') AND b.skill_id=json_extract(ref.value,'$.skillId')) AND NOT EXISTS(SELECT 1 FROM skill_bindings b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.deployment_id=json_extract(ref.value,'$.assignedAgentId') AND b.skill_id=json_extract(ref.value,'$.skillId') AND b.skill_version=json_extract(ref.value,'$.skillVersion'))) THEN RAISE(ABORT,'selected Project Skill executable version is unavailable') END;
		END`
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
		statement = `CREATE TRIGGER IF NOT EXISTS project_skill_references_` + operation.name + ` AFTER ` + operation.event + ` ON projects BEGIN DELETE FROM project_skill_references WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND project_id=NEW.id; INSERT INTO project_skill_references ` + projection("NEW") + `; END`
		if operation.name == "update" {
			statement = `CREATE TRIGGER IF NOT EXISTS project_skill_references_update AFTER UPDATE ON projects BEGIN DELETE FROM project_skill_references WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND project_id=OLD.id; DELETE FROM project_skill_references WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND project_id=NEW.id; INSERT INTO project_skill_references ` + projection("NEW") + `; END`
		}
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE TRIGGER IF NOT EXISTS delete_project_skill_references AFTER DELETE ON projects BEGIN DELETE FROM project_skill_references WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND project_id=OLD.id; END`); err != nil {
		return err
	}
	return tx.Commit()
}

func postgresProjectSkillMonitors(payload string) string {
	return `jsonb_array_elements(CASE WHEN jsonb_typeof(` + payload + `->'sourceMonitors')='array' THEN ` + payload + `->'sourceMonitors' ELSE '[]'::jsonb END)`
}

func postgresProjectSkillMonitorValid(alias string) string {
	checks := []string{`jsonb_typeof(` + alias + `)='object'`}
	for _, field := range []string{"id", "assignedAgentId", "skillId", "skillVersion", "action"} {
		checks = append(checks, `jsonb_typeof(`+alias+`->'`+field+`')='string'`, `length(btrim(`+alias+`->>'`+field+`'))>0`, `octet_length(`+alias+`->>'`+field+`')<=128`)
	}
	return "COALESCE((" + strings.Join(checks, " AND ") + "),false)"
}

func (s *PostgresStore) migrateProjectSkillReferences(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, projectSkillReferenceMigrationVersion)
	if err != nil || applied {
		return err
	}
	statements := []string{
		// Hold legacy writers out between the backfill snapshot and trigger
		// installation. This table lock lasts only for the migration transaction.
		`LOCK TABLE ` + s.table("projects") + ` IN SHARE ROW EXCLUSIVE MODE`,
		`CREATE TABLE IF NOT EXISTS ` + s.table("project_skill_references") + `(scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,project_id TEXT COLLATE "C" NOT NULL,monitor_id TEXT NOT NULL,project_revision BIGINT NOT NULL,owner_id TEXT NOT NULL,assigned_agent_id TEXT NOT NULL,skill_id TEXT NOT NULL,skill_version TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,project_id,monitor_id))`,
		`CREATE INDEX IF NOT EXISTS project_skill_references_assigned ON ` + s.table("project_skill_references") + `(scope_kind,scope_id,skill_id,skill_version,assigned_agent_id,project_id,monitor_id,project_revision)`,
		`CREATE INDEX IF NOT EXISTS project_skill_references_owner ON ` + s.table("project_skill_references") + `(scope_kind,scope_id,skill_id,skill_version,owner_id,project_id,monitor_id,project_revision)`,
		`INSERT INTO ` + s.table("project_skill_references") + ` SELECT p.scope_kind,p.scope_id,p.id,ref->>'id',p.revision,p.owner_id,ref->>'assignedAgentId',ref->>'skillId',ref->>'skillVersion' FROM ` + s.table("projects") + ` p CROSS JOIN LATERAL ` + postgresProjectSkillMonitors("p.payload") + ` ref WHERE ` + postgresProjectSkillMonitorValid("ref") + ` ON CONFLICT DO NOTHING`,
		`CREATE OR REPLACE FUNCTION ` + s.table("admit_project_skill_references") + `() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE previous_payload JSONB='{}'::jsonb; current_payload JSONB='{}'::jsonb; previous_owner TEXT=''; previous_scope_kind TEXT=''; previous_scope_id TEXT=''; current_scope_kind TEXT=''; current_scope_id TEXT=''; logical RECORD; monitor_record JSONB; lock_key TEXT;
		BEGIN
		IF TG_OP<>'INSERT' THEN previous_payload=OLD.payload; previous_owner=OLD.owner_id; previous_scope_kind=OLD.scope_kind; previous_scope_id=OLD.scope_id; END IF;
		IF TG_OP<>'DELETE' THEN current_payload=NEW.payload; current_scope_kind=NEW.scope_kind; current_scope_id=NEW.scope_id; END IF;
		FOR logical IN SELECT scope_kind,scope_id,skill_id FROM (SELECT previous_scope_kind AS scope_kind,previous_scope_id AS scope_id,value->>'skillId' AS skill_id FROM ` + postgresProjectSkillMonitors("previous_payload") + ` UNION SELECT current_scope_kind,current_scope_id,value->>'skillId' FROM ` + postgresProjectSkillMonitors("current_payload") + `) refs WHERE skill_id IS NOT NULL AND skill_id<>'' ORDER BY scope_kind COLLATE "C",scope_id COLLATE "C",skill_id COLLATE "C" LOOP
		lock_key='skill-runtime-maintenance:'||octet_length(logical.scope_kind)::text||':'||logical.scope_kind||':'||octet_length(logical.scope_id)::text||':'||logical.scope_id||':'||octet_length(logical.skill_id)::text||':'||logical.skill_id;
		PERFORM pg_advisory_xact_lock_shared(hashtextextended(lock_key,0)); END LOOP;
		IF TG_OP='DELETE' THEN RETURN OLD; END IF;
		IF (current_payload ? 'sourceMonitors' AND jsonb_typeof(current_payload->'sourceMonitors') NOT IN ('array','null')) OR EXISTS(SELECT 1 FROM ` + postgresProjectSkillMonitors("current_payload") + ` ref WHERE NOT (` + postgresProjectSkillMonitorValid("ref") + `)) OR EXISTS(SELECT 1 FROM ` + postgresProjectSkillMonitors("current_payload") + ` ref GROUP BY ref->>'id' HAVING count(*)>1) THEN RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='Project source monitor identity is invalid'; END IF;
		FOR monitor_record IN SELECT candidate.value FROM ` + postgresProjectSkillMonitors("current_payload") + ` candidate WHERE TG_OP='INSERT' OR NEW.owner_id<>previous_owner OR current_scope_kind<>previous_scope_kind OR current_scope_id<>previous_scope_id OR NOT EXISTS(SELECT 1 FROM ` + postgresProjectSkillMonitors("previous_payload") + ` prior WHERE prior.value->>'id'=candidate.value->>'id' AND prior.value=candidate.value) LOOP
		IF EXISTS(SELECT 1 FROM ` + s.table("skill_bindings") + ` b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.deployment_id=monitor_record->>'assignedAgentId' AND b.skill_id=monitor_record->>'skillId') AND NOT EXISTS(SELECT 1 FROM ` + s.table("skill_bindings") + ` b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.deployment_id=monitor_record->>'assignedAgentId' AND b.skill_id=monitor_record->>'skillId' AND b.skill_version=monitor_record->>'skillVersion') THEN RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='selected Project Skill executable version is unavailable'; END IF; END LOOP;
		RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS admit_project_skill_references ON ` + s.table("projects") + `; CREATE TRIGGER admit_project_skill_references BEFORE INSERT OR UPDATE OR DELETE ON ` + s.table("projects") + ` FOR EACH ROW EXECUTE FUNCTION ` + s.table("admit_project_skill_references") + `()`,
		`CREATE OR REPLACE FUNCTION ` + s.table("project_skill_references") + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF TG_OP<>'INSERT' THEN DELETE FROM ` + s.table("project_skill_references") + ` WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND project_id=OLD.id; END IF;
		IF TG_OP='DELETE' THEN RETURN OLD; END IF;
		DELETE FROM ` + s.table("project_skill_references") + ` WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND project_id=NEW.id;
		INSERT INTO ` + s.table("project_skill_references") + ` SELECT NEW.scope_kind,NEW.scope_id,NEW.id,ref->>'id',NEW.revision,NEW.owner_id,ref->>'assignedAgentId',ref->>'skillId',ref->>'skillVersion' FROM ` + postgresProjectSkillMonitors("NEW.payload") + ` ref WHERE ` + postgresProjectSkillMonitorValid("ref") + `;
		RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS project_skill_references ON ` + s.table("projects") + `; CREATE TRIGGER project_skill_references AFTER INSERT OR UPDATE OR DELETE ON ` + s.table("projects") + ` FOR EACH ROW EXECUTE FUNCTION ` + s.table("project_skill_references") + `()`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'indexed Project Skill reference admission')`, projectSkillReferenceMigrationVersion)
	return err
}
