package runtime

import (
	"context"
	"database/sql"
)

const runSkillDependencyMigrationVersion int64 = 54

// Canonical Run triggers cover older binaries during a rolling upgrade. Only
// initial migration reads existing context; reconciliation uses indexed metadata.
func migrateRunSkillDependenciesSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='run_skill_dependencies')`).Scan(&exists); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS run_skill_dependencies(scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,run_id TEXT NOT NULL,skill_id TEXT NOT NULL,skill_version TEXT NOT NULL,deployment_id TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,run_id,skill_id,skill_version,deployment_id))`,
		`CREATE INDEX IF NOT EXISTS idx_run_skill_dependencies_runtime ON run_skill_dependencies(scope_kind,scope_id,skill_id,skill_version,deployment_id)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_runs_legacy_skill_dependency_page ON agent_runs(scope_kind,scope_id,id) WHERE status IN (` + unfinishedSkillRunSQL + `)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if !exists {
		p := sqliteDependencyPayload("r")
		statement := `INSERT OR IGNORE INTO run_skill_dependencies
   SELECT r.scope_kind,r.scope_id,r.id,trim(json_extract(` + p + `,'$.context.capabilityInvocation.skillId')),trim(json_extract(` + p + `,'$.context.capabilityInvocation.skillVersion')),CASE WHEN json_extract(` + p + `,'$.owner.type')='team' THEN '' ELSE r.assigned_agent_id END
   FROM agent_runs r WHERE r.status IN (` + unfinishedSkillRunSQL + `) AND ` + sqliteTypedDependencyValid(p) + `
   UNION SELECT r.scope_kind,r.scope_id,r.id,trim(json_extract(ref.value,'$.skillId')),trim(json_extract(ref.value,'$.skillVersion')),CASE WHEN json_extract(` + p + `,'$.owner.type')='team' THEN '' ELSE r.assigned_agent_id END
   FROM agent_runs r,json_each(` + p + `,'$.context.acceptedRunExecution.skillDependencies') ref
   WHERE r.status IN (` + unfinishedSkillRunSQL + `) AND ` + sqliteAcceptedDependencyValid("r", p) + ` AND ` + sqliteDependencyReferenceValid() + `;`
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
		var registryPresent bool
		if err := tx.QueryRow(`SELECT COUNT(*)=2 FROM sqlite_master WHERE type='table' AND name IN ('agent_deployments','agent_definitions')`).Scan(&registryPresent); err != nil {
			return err
		}
		if registryPresent {
			if err := backfillLegacyRunSkillDependencies(context.Background(), tx, "agent_runs", "agent_deployments", "agent_definitions", "run_skill_dependencies", false); err != nil {
				return err
			}
		}
	}
	for _, operation := range []struct{ name, event string }{{"insert", "INSERT"}, {"update", "UPDATE OF status,payload,assigned_agent_id"}} {
		candidates := sqliteRunSkillDependencyCandidates("NEW")
		pending := `SELECT c.skill_id,c.skill_version FROM (` + candidates + `) c WHERE NOT EXISTS(SELECT 1 FROM run_skill_dependencies d WHERE d.scope_kind=NEW.scope_kind AND d.scope_id=NEW.scope_id AND d.run_id=NEW.id AND d.skill_id=c.skill_id AND d.skill_version=c.skill_version AND d.deployment_id=` + sqliteDependencyDeployment("NEW") + `)`
		immutable := ""
		if operation.name == "update" {
			immutable = `SELECT CASE WHEN json_extract(` + sqliteDependencyPayload("OLD") + `,'$.context.acceptedRunExecution') IS NOT NULL AND (` + sqliteAcceptedExecutionTreeDifference("OLD", "NEW") + ` OR ` + sqliteAcceptedExecutionTreeDifference("NEW", "OLD") + `) THEN RAISE(ABORT,'accepted Run execution identity is unavailable or invalid') END;`
		}
		statement := `CREATE TRIGGER IF NOT EXISTS project_run_skill_dependencies_` + operation.name + ` AFTER ` + operation.event + ` ON agent_runs BEGIN
   ` + immutable + `
   SELECT CASE WHEN NEW.status IN (` + unfinishedSkillRunSQL + `) AND EXISTS(SELECT 1 FROM (` + pending + `) c JOIN skill_runtime_maintenance m ON m.scope_kind=NEW.scope_kind AND m.scope_id=NEW.scope_id AND m.skill_id=c.skill_id AND m.active=1) THEN RAISE(ABORT,'Skill runtime maintenance is in progress') END;
   SELECT CASE WHEN NEW.status IN (` + unfinishedSkillRunSQL + `) AND EXISTS(SELECT 1 FROM (` + pending + `) c
    WHERE EXISTS(SELECT 1 FROM skill_bindings b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.skill_id=c.skill_id AND (` + sqliteDependencyDeployment("NEW") + `='' OR b.deployment_id=` + sqliteDependencyDeployment("NEW") + `))
    AND NOT EXISTS(SELECT 1 FROM skill_bindings b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.skill_id=c.skill_id AND (` + sqliteDependencyDeployment("NEW") + `='' OR b.deployment_id=` + sqliteDependencyDeployment("NEW") + `) AND b.skill_version=c.skill_version AND COALESCE(json_extract(b.payload,'$.disabled'),0)=0))
    THEN RAISE(ABORT,'selected Skill executable version is unavailable') END;
   DELETE FROM run_skill_dependencies WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND run_id=NEW.id AND NEW.status NOT IN (` + unfinishedSkillRunSQL + `);
   INSERT OR IGNORE INTO run_skill_dependencies SELECT NEW.scope_kind,NEW.scope_id,NEW.id,c.skill_id,c.skill_version,` + sqliteDependencyDeployment("NEW") + ` FROM (` + candidates + `) c WHERE NEW.status IN (` + unfinishedSkillRunSQL + `);
   END`
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE TRIGGER IF NOT EXISTS delete_run_skill_dependencies AFTER DELETE ON agent_runs BEGIN DELETE FROM run_skill_dependencies WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND run_id=OLD.id; END`); err != nil {
		return err
	}
	return tx.Commit()
}

// JSON member order changes when a persisted map is decoded and re-encoded.
// Compare the bounded typed snapshot's semantic nodes, including array order,
// rather than treating those harmless formatting changes as a new method.
func sqliteAcceptedExecutionTreeDifference(a, b string) string {
	return `EXISTS(SELECT fullkey,type,atom FROM json_tree(` + sqliteDependencyPayload(a) + `,'$.context.acceptedRunExecution') EXCEPT SELECT fullkey,type,atom FROM json_tree(` + sqliteDependencyPayload(b) + `,'$.context.acceptedRunExecution'))`
}

func sqliteDependencyDeployment(alias string) string {
	return `CASE WHEN json_extract(` + sqliteDependencyPayload(alias) + `,'$.owner.type')='team' THEN '' ELSE ` + alias + `.assigned_agent_id END`
}

func sqliteDependencyPayload(alias string) string {
	return `(CASE WHEN json_valid(` + alias + `.payload) THEN ` + alias + `.payload ELSE '{}' END)`
}
func sqliteTypedDependencyValid(p string) string {
	return `json_type(` + p + `,'$.context.capabilityInvocation.skillId')='text' AND json_type(` + p + `,'$.context.capabilityInvocation.skillVersion')='text' AND trim(json_extract(` + p + `,'$.context.capabilityInvocation.skillId'))<>'' AND trim(json_extract(` + p + `,'$.context.capabilityInvocation.skillVersion'))<>''`
}
func sqliteAcceptedDependencyValid(alias, p string) string {
	return `json_type(` + p + `,'$.context.acceptedRunExecution.skillDependencies')='array' AND json_array_length(` + p + `,'$.context.acceptedRunExecution.skillDependencies')<=1024 AND json_extract(` + p + `,'$.context.acceptedRunExecution.scope.kind')=` + alias + `.scope_kind AND json_extract(` + p + `,'$.context.acceptedRunExecution.scope.id')=` + alias + `.scope_id AND json_extract(` + p + `,'$.context.acceptedRunExecution.deploymentId')=` + alias + `.assigned_agent_id`
}
func sqliteDependencyReferenceValid() string {
	return `ref.type='object' AND json_type(ref.value,'$.skillId')='text' AND json_type(ref.value,'$.skillVersion')='text' AND trim(json_extract(ref.value,'$.skillId'))<>'' AND trim(json_extract(ref.value,'$.skillVersion'))<>''`
}
func sqliteRunSkillDependencyCandidates(alias string) string {
	p := sqliteDependencyPayload(alias)
	return `SELECT trim(json_extract(` + p + `,'$.context.capabilityInvocation.skillId')) skill_id,trim(json_extract(` + p + `,'$.context.capabilityInvocation.skillVersion')) skill_version WHERE ` + sqliteTypedDependencyValid(p) + `
 UNION SELECT trim(json_extract(ref.value,'$.skillId')),trim(json_extract(ref.value,'$.skillVersion')) FROM json_each(` + p + `,'$.context.acceptedRunExecution.skillDependencies') ref WHERE ` + sqliteAcceptedDependencyValid(alias, p) + ` AND ` + sqliteDependencyReferenceValid()
}

func (s *PostgresStore) migrateRunSkillDependencies(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, runSkillDependencyMigrationVersion)
	if err != nil || applied {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + s.table("run_skill_dependencies") + `(scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,run_id TEXT NOT NULL,skill_id TEXT NOT NULL,skill_version TEXT NOT NULL,deployment_id TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,run_id,skill_id,skill_version,deployment_id))`,
		`CREATE INDEX IF NOT EXISTS run_skill_dependencies_runtime ON ` + s.table("run_skill_dependencies") + `(scope_kind,scope_id,skill_id,skill_version,deployment_id)`,
		`CREATE INDEX IF NOT EXISTS agent_runs_legacy_skill_dependency_page ON ` + s.table("agent_runs") + `(scope_kind,scope_id,id) WHERE status IN (` + unfinishedSkillRunSQL + `)`,
		`INSERT INTO ` + s.table("run_skill_dependencies") + `
   SELECT r.scope_kind,r.scope_id,r.id,c.skill_id,c.skill_version,CASE WHEN r.payload#>>'{owner,type}'='team' THEN '' ELSE r.assigned_agent_id END FROM ` + s.table("agent_runs") + ` r CROSS JOIN LATERAL (` + postgresRunSkillDependencyCandidates("r") + `) c WHERE r.status IN (` + unfinishedSkillRunSQL + `) ON CONFLICT DO NOTHING`,
		`CREATE OR REPLACE FUNCTION ` + s.table("project_run_skill_dependencies") + `() RETURNS trigger LANGUAGE plpgsql AS $$
  DECLARE dependency RECORD; lock_key TEXT; authority_deployment TEXT;
  BEGIN
   IF TG_OP='DELETE' THEN DELETE FROM ` + s.table("run_skill_dependencies") + ` WHERE scope_kind=OLD.scope_kind AND scope_id=OLD.scope_id AND run_id=OLD.id;RETURN OLD;END IF;
   IF TG_OP='UPDATE' AND OLD.payload#>'{context,acceptedRunExecution}' IS NOT NULL AND (OLD.payload#>'{context,acceptedRunExecution}') IS DISTINCT FROM (NEW.payload#>'{context,acceptedRunExecution}') THEN RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='accepted Run execution identity is unavailable or invalid';END IF;
   IF NEW.status NOT IN (` + unfinishedSkillRunSQL + `) THEN DELETE FROM ` + s.table("run_skill_dependencies") + ` WHERE scope_kind=NEW.scope_kind AND scope_id=NEW.scope_id AND run_id=NEW.id;RETURN NEW;END IF;
   authority_deployment=CASE WHEN NEW.payload#>>'{owner,type}'='team' THEN '' ELSE NEW.assigned_agent_id END;
   FOR dependency IN ` + postgresRunSkillDependencyCandidates("NEW") + ` ORDER BY 1,2 LOOP
    IF NOT EXISTS(SELECT 1 FROM ` + s.table("run_skill_dependencies") + ` d WHERE d.scope_kind=NEW.scope_kind AND d.scope_id=NEW.scope_id AND d.run_id=NEW.id AND d.skill_id=dependency.skill_id AND d.skill_version=dependency.skill_version AND d.deployment_id=authority_deployment) THEN
     lock_key='skill-runtime-maintenance:'||octet_length(NEW.scope_kind)::text||':'||NEW.scope_kind||':'||octet_length(NEW.scope_id)::text||':'||NEW.scope_id||':'||octet_length(dependency.skill_id)::text||':'||dependency.skill_id;
     PERFORM pg_advisory_xact_lock_shared(hashtextextended(lock_key,0));
     IF EXISTS(SELECT 1 FROM ` + s.table("skill_runtime_maintenance") + ` m WHERE m.scope_kind=NEW.scope_kind AND m.scope_id=NEW.scope_id AND m.skill_id=dependency.skill_id AND m.active) THEN RAISE EXCEPTION USING ERRCODE='55P03',MESSAGE='Skill runtime maintenance is in progress';END IF;
     IF EXISTS(SELECT 1 FROM ` + s.table("skill_bindings") + ` b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.skill_id=dependency.skill_id AND (authority_deployment='' OR b.deployment_id=authority_deployment)) AND NOT EXISTS(SELECT 1 FROM ` + s.table("skill_bindings") + ` b WHERE b.scope_kind=NEW.scope_kind AND b.scope_id=NEW.scope_id AND b.skill_id=dependency.skill_id AND (authority_deployment='' OR b.deployment_id=authority_deployment) AND b.skill_version=dependency.skill_version AND COALESCE((b.payload->>'disabled')::boolean,false)=false) THEN RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='selected Skill executable version is unavailable';END IF;
     INSERT INTO ` + s.table("run_skill_dependencies") + ` VALUES(NEW.scope_kind,NEW.scope_id,NEW.id,dependency.skill_id,dependency.skill_version,authority_deployment) ON CONFLICT DO NOTHING;
    END IF;
   END LOOP;
   RETURN NEW;
  END $$`,
		`DROP TRIGGER IF EXISTS project_run_skill_dependencies ON ` + s.table("agent_runs") + `; CREATE TRIGGER project_run_skill_dependencies AFTER INSERT OR UPDATE OF status,payload,assigned_agent_id OR DELETE ON ` + s.table("agent_runs") + ` FOR EACH ROW EXECUTE FUNCTION ` + s.table("project_run_skill_dependencies") + `()`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := backfillLegacyRunSkillDependencies(ctx, tx, s.table("agent_runs"), s.table("agent_deployments"), s.table("agent_definitions"), s.table("run_skill_dependencies"), true); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'indexed accepted Run Skill dependencies')`, runSkillDependencyMigrationVersion)
	return err
}

func postgresRunSkillDependencyCandidates(alias string) string {
	p := alias + `.payload`
	return `SELECT btrim(` + p + `#>>'{context,capabilityInvocation,skillId}') skill_id,btrim(` + p + `#>>'{context,capabilityInvocation,skillVersion}') skill_version WHERE jsonb_typeof(` + p + `#>'{context,capabilityInvocation,skillId}')='string' AND jsonb_typeof(` + p + `#>'{context,capabilityInvocation,skillVersion}')='string' AND btrim(` + p + `#>>'{context,capabilityInvocation,skillId}')<>'' AND btrim(` + p + `#>>'{context,capabilityInvocation,skillVersion}')<>''
 UNION SELECT btrim(ref->>'skillId'),btrim(ref->>'skillVersion') FROM jsonb_array_elements(CASE WHEN jsonb_typeof(` + p + `#>'{context,acceptedRunExecution,skillDependencies}')='array' AND jsonb_array_length(` + p + `#>'{context,acceptedRunExecution,skillDependencies}')<=1024 THEN ` + p + `#>'{context,acceptedRunExecution,skillDependencies}' ELSE '[]'::jsonb END) ref
 WHERE jsonb_typeof(ref->'skillId')='string' AND jsonb_typeof(ref->'skillVersion')='string' AND btrim(ref->>'skillId')<>'' AND btrim(ref->>'skillVersion')<>'' AND ` + p + `#>>'{context,acceptedRunExecution,scope,kind}'=` + alias + `.scope_kind AND ` + p + `#>>'{context,acceptedRunExecution,scope,id}'=` + alias + `.scope_id AND ` + p + `#>>'{context,acceptedRunExecution,deploymentId}'=` + alias + `.assigned_agent_id`
}
