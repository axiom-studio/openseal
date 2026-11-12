package sqlite

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// Store tests: the SQLite store's skill runtime usage projections, their
// legacy migrations and query plans.

func TestSkillUpgradeDrainQueriesUseIndexedMetadataWithoutPayloads(t *testing.T) {
	store, err := NewSQLiteStore(t.TempDir() + "/metadata.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Malformed action payloads make an accidental decode fail immediately.
	_, err = store.db.Exec(`INSERT INTO action_calls(id,scope_kind,scope_id,run_id,status,available_at,revision,created_at,payload,deployment_id,binding_id,binding_revision,skill_id,skill_version) VALUES ('active','tenant','metadata','run','ready',CURRENT_TIMESTAMP,1,CURRENT_TIMESTAMP,'not-json','agent','account',1,'skill','1')`)
	if err != nil {
		t.Fatal(err)
	}
	filter := models.SkillRuntimeUsageFilter{Scope: models.Scope{Kind: "tenant", ID: "metadata"}, SkillID: "skill", SkillVersion: "1", DeploymentID: "agent", BindingID: "account"}
	if busy, err := store.HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
		t.Fatalf("metadata query decoded payload: %v %v", busy, err)
	}
	refs, err := store.ListReferencedSkillRuntimeVersions(t.Context(), filter.Scope)
	if err != nil || len(refs) != 1 || refs[0].SourceIdentity != "" {
		t.Fatalf("metadata retention: %#v %v", refs, err)
	}
	for _, selected := range []models.SkillRuntimeUsageFilter{filter, {Scope: filter.Scope, SkillID: filter.SkillID, SkillVersion: filter.SkillVersion}} {
		query, args := sqlstore.SkillRuntimeUsageQuery(selected, "action_calls", "agent_runs", false)
		plan := sqliteSkillUsageExplain(t, store.db, query, args...)
		activeIndex := "idx_action_calls_skill_runtime_active"
		if selected.BindingID != "" {
			activeIndex = "idx_action_calls_skill_binding_active"
		}
		for _, index := range []string{activeIndex, "idx_agent_runs_skill_runtime_active", "idx_action_calls_skill_run_receipt"} {
			if !strings.Contains(plan, index) {
				t.Fatalf("missing %s: %s", index, plan)
			}
		}
		if strings.Contains(plan, "SCAN a") || strings.Contains(plan, "SCAN r") {
			t.Fatalf("unbounded action/run scan: %s", plan)
		}
	}
	plan := sqliteSkillUsageExplain(t, store.db, sqlstore.ListReferencedSkillRuntimeVersionsQuery("skill_bindings", "action_calls", "agent_runs", false), filter.Scope.Kind, filter.Scope.ID)
	for _, index := range []string{"idx_agent_runs_skill_runtime_active", "idx_action_calls_skill_run_receipt"} {
		if !strings.Contains(plan, index) {
			t.Fatalf("retention missing %s: %s", index, plan)
		}
	}
	if !strings.Contains(plan, "idx_action_calls_skill_runtime_active") && !strings.Contains(plan, "idx_action_calls_skill_binding_active") {
		t.Fatalf("retention does not use a partial unfinished action index: %s", plan)
	}
	if strings.Contains(plan, "SCAN a") || strings.Contains(plan, "SCAN r") {
		t.Fatalf("retention scans terminal history: %s", plan)
	}
}

func sqliteSkillUsageExplain(t *testing.T, db *sql.DB, query string, args ...interface{}) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func TestSkillUpgradeDrainMigrationProjectsLegacyMetadata(t *testing.T) {
	db, err := sql.Open("sqlite3", t.TempDir()+"/legacy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE action_calls(id TEXT,scope_kind TEXT,scope_id TEXT,run_id TEXT,status TEXT,payload TEXT);CREATE TABLE agent_runs(id TEXT,scope_kind TEXT,scope_id TEXT,status TEXT);CREATE TABLE skill_bindings(scope_kind TEXT,scope_id TEXT,deployment_id TEXT,id TEXT,revision INTEGER,skill_id TEXT,skill_version TEXT,payload TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	call := models.ActionCall{ID: "old", Scope: models.Scope{Kind: "tenant", ID: "one"}, RunID: "run", DeploymentID: "agent", BindingID: "account", BindingRevision: 7, SkillID: "skill", SkillVersion: "2", Status: models.ActionCallStatusReady}
	payload, _ := json.Marshal(call)
	if _, err = db.Exec(`INSERT INTO action_calls VALUES(?,?,?,?,?,?)`, call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.Status, string(payload)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = migrateSkillUpgradeDrainSQLite(db); err != nil {
			t.Fatal(err)
		}
	}
	var deployment, binding, id, version string
	var revision int64
	if err = db.QueryRow(`SELECT deployment_id,binding_id,binding_revision,skill_id,skill_version FROM action_calls`).Scan(&deployment, &binding, &revision, &id, &version); err != nil {
		t.Fatal(err)
	}
	if deployment != call.DeploymentID || binding != call.BindingID || revision != 7 || id != call.SkillID || version != call.SkillVersion {
		t.Fatalf("legacy projection: %s %s %d %s %s", deployment, binding, revision, id, version)
	}
	call.ID = "rolling-writer"
	payload, _ = json.Marshal(call)
	if _, err = db.Exec(`INSERT INTO action_calls(id,scope_kind,scope_id,run_id,status,payload) VALUES(?,?,?,?,?,?)`, call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.Status, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT deployment_id,binding_id,binding_revision,skill_id,skill_version FROM action_calls WHERE id=?`, call.ID).Scan(&deployment, &binding, &revision, &id, &version); err != nil {
		t.Fatal(err)
	}
	if deployment != call.DeploymentID || binding != call.BindingID || revision != 7 || id != call.SkillID || version != call.SkillVersion {
		t.Fatalf("rolling writer metadata was missing: %s %s %d %s %s", deployment, binding, revision, id, version)
	}
}

func TestRunSkillDependencySQLiteProjectionMigrationAndMetadataQueries(t *testing.T) {
	db, err := sql.Open("sqlite3", t.TempDir()+"/legacy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE agent_runs(scope_kind TEXT,scope_id TEXT,id TEXT,status TEXT,assigned_agent_id TEXT,payload TEXT); CREATE TABLE skill_runtime_maintenance(scope_kind TEXT,scope_id TEXT,skill_id TEXT,active INTEGER)`); err != nil {
		t.Fatal(err)
	}
	run := &models.AgentRun{Scope: models.Scope{Kind: "tenant", ID: "legacy"}, ID: "accepted", AssignedAgentID: "agent", Status: models.AgentRunStatusSleeping,
		Context: map[string]interface{}{models.CapabilityInvocationContextKey: map[string]interface{}{"skillId": "reader", "skillVersion": "1.0.0", "action": "read", "inputs": map[string]interface{}{"scope": map[string]interface{}{"kind": "tenant", "id": "untrusted"}}}}}
	payload, _ := json.Marshal(run)
	for _, status := range []string{"sleeping", "completed"} {
		if _, err := db.Exec(`INSERT INTO agent_runs VALUES('tenant','legacy',?,?,?,?)`, status, status, "agent", string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateRunSkillDependenciesSQLite(db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_skill_dependencies`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy migration retained wrong Runs: %d %v", count, err)
	}
	// A reopen must not read arbitrary historical JSON again.
	if _, err := db.Exec(`DROP TRIGGER project_run_skill_dependencies_update; UPDATE agent_runs SET payload='not-json'`); err != nil {
		t.Fatal(err)
	}
	if err := migrateRunSkillDependenciesSQLite(db); err != nil {
		t.Fatalf("reopen scanned historical payload: %v", err)
	}
	plan := sqliteSkillUsageExplain(t, db, `SELECT 1 FROM run_skill_dependencies WHERE scope_kind=? AND scope_id=? AND skill_id=? AND skill_version=? AND deployment_id IN (?,'') LIMIT 1`, "tenant", "legacy", "reader", "1.0.0", "agent")
	if !strings.Contains(plan, "idx_run_skill_dependencies_runtime") || strings.Contains(plan, "SCAN ") {
		t.Fatalf("typed dependency probe is not indexed: %s", plan)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_skill_dependencies WHERE scope_kind='tenant' AND scope_id='legacy' AND skill_id='reader' AND skill_version='1.0.0'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("projection required Run payload decode: %d %v", count, err)
	}
}
