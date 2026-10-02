package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func projectSkillMetadataSQLStore(t *testing.T, fixture eventWaitContractFixture) (*sql.DB, string, bool) {
	t.Helper()
	switch store := fixture.store.(type) {
	case *SQLiteStore:
		return store.db, "projects", false
	case *PostgresStore:
		return store.db, store.table("projects"), true
	default:
		t.Skip("legacy SQL writer contract applies to durable SQL stores")
		return nil, "", false
	}
}

func writeProjectSkillMetadataSQL(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}, table string, project *Project, insert, postgres bool) error {
	payload, err := json.Marshal(project)
	if err != nil {
		return err
	}
	parameter := func(n int) string { return projectSkillReferenceParameter(n, postgres) }
	cast := ""
	if postgres {
		cast = "::jsonb"
	}
	if !insert {
		_, err = q.ExecContext(ctx, `UPDATE `+table+` SET revision=`+parameter(1)+`,payload=`+parameter(2)+cast+`,owner_id=`+parameter(3)+` WHERE scope_kind=`+parameter(4)+` AND scope_id=`+parameter(5)+` AND id=`+parameter(6), project.Revision, string(payload), project.Owner.ID, project.Scope.Kind, project.Scope.ID, project.ID)
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO `+table+`(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,payload) VALUES(`+parameter(1)+`,`+parameter(2)+`,`+parameter(3)+`,`+parameter(4)+`,`+parameter(5)+`,`+parameter(6)+`,`+parameter(7)+`,`+parameter(8)+`,`+parameter(9)+cast+`)`, project.ID, project.Scope.Kind, project.Scope.ID, project.Owner.Type, project.Owner.ID, project.Status, project.Revision, project.UpdatedAt, string(payload))
	return err
}

func TestProjectSkillReferenceLegacySQLWritersCannotRestoreObsoleteVersion(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		db, projects, postgres := projectSkillMetadataSQLStore(t, fixture)
		store := fixture.store.(SkillReferenceUpgradeStore)
		scope := Scope{Kind: "tenant", ID: "project-sql-admission"}
		project := projectFixture(scope)
		binding := &skill.Binding{ID: "source", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: project.SourceMonitors[0].AssignedAgentID,
			SkillID: project.SourceMonitors[0].SkillID, SkillVersion: "1.0.0", AllowedActions: []string{"search"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}
		if err := store.SaveSkillBinding(t.Context(), binding, 0); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateProject(t.Context(), project); err != nil {
			t.Fatal(err)
		}
		// Simulate a completed binding cutover before an older binary resumes.
		updated := *binding
		updated.SkillVersion, updated.Revision = "1.1.0", 2
		if err := store.SaveSkillBinding(t.Context(), &updated, 1); err != nil {
			t.Fatal(err)
		}
		metadata := cloneProject(project)
		metadata.Title, metadata.Revision = "Historical source configuration", 2
		if err := writeProjectSkillMetadataSQL(t.Context(), db, projects, metadata, false, postgres); err != nil {
			t.Fatalf("unchanged historical monitor blocked metadata: %v", err)
		}
		stale := cloneProject(project)
		stale.ID = "late-obsolete-project"
		if err := writeProjectSkillMetadataSQL(t.Context(), db, projects, stale, true, postgres); !errors.Is(projectSkillReferenceAdmissionError(err), ErrProjectSkillReferenceUnavailable) {
			t.Fatalf("legacy INSERT restored obsolete monitor: %v", err)
		}
		stale = cloneProject(metadata)
		stale.Revision++
		stale.SourceMonitors[0].SourcePolicyRef = "changed-source-selection"
		if err := writeProjectSkillMetadataSQL(t.Context(), db, projects, stale, false, postgres); !errors.Is(projectSkillReferenceAdmissionError(err), ErrProjectSkillReferenceUnavailable) {
			t.Fatalf("legacy UPDATE changed an obsolete monitor: %v", err)
		}
		current := cloneProject(stale)
		current.SourceMonitors[0].SkillVersion = updated.SkillVersion
		if err := writeProjectSkillMetadataSQL(t.Context(), db, projects, current, false, postgres); err != nil {
			t.Fatalf("current exact version was rejected: %v", err)
		}
		// Dormant configuration retains an exact grant without enabling it.
		updated.Disabled, updated.Revision = true, 3
		if err := store.SaveSkillBinding(t.Context(), &updated, 2); err != nil {
			t.Fatal(err)
		}
		dormant := cloneProject(current)
		dormant.ID, dormant.Revision = "dormant-exact-project", 1
		if err := writeProjectSkillMetadataSQL(t.Context(), db, projects, dormant, true, postgres); err != nil {
			t.Fatalf("disabled exact configuration version was rejected: %v", err)
		}
		dormant.ID = "dormant-obsolete-project"
		dormant.SourceMonitors[0].SkillVersion = "1.0.0"
		if err := writeProjectSkillMetadataSQL(t.Context(), db, projects, dormant, true, postgres); !errors.Is(projectSkillReferenceAdmissionError(err), ErrProjectSkillReferenceUnavailable) {
			t.Fatalf("disabled binding admitted an obsolete version: %v", err)
		}
		bindings, err := store.ListSkillBindings(t.Context(), binding.Scope, binding.DeploymentID)
		if err != nil || len(bindings) != 1 || !bindings[0].Disabled || bindings[0].Revision != 3 {
			t.Fatalf("metadata admission changed execution authority: %#v err=%v", bindings, err)
		}
	})
}

func TestProjectSkillReferenceProjectionScopesOwnersAssigneesAndUnicodePages(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(SkillReferenceUpgradeStore)
		scope := Scope{Kind: "tenant", ID: "project-index-scope"}
		matching := []string{"assigned-only", "both", "owner-only", "z-project", "Ä-project", "é-project", "🦭-project"}
		for _, id := range matching {
			project := projectFixture(scope)
			project.ID, project.Owner.ID = id, "other-owner"
			project.SourceMonitors[0].AssignedAgentID = "agent-a"
			if id == "owner-only" || id == "both" {
				project.Owner.ID = "agent-a"
			}
			if id == "owner-only" {
				project.SourceMonitors[0].AssignedAgentID = "other-agent"
			}
			if err := store.CreateProject(t.Context(), project); err != nil {
				t.Fatal(err)
			}
		}
		for _, kind := range []string{"different-scope", "different-agent", "different-skill", "different-version"} {
			project := projectFixture(scope)
			project.ID, project.Owner.ID, project.SourceMonitors[0].AssignedAgentID = kind, "agent-a", "agent-a"
			switch kind {
			case "different-scope":
				project.Scope.ID = "another-tenant"
			case "different-agent":
				project.Owner.ID, project.SourceMonitors[0].AssignedAgentID = "other", "other"
			case "different-skill":
				project.SourceMonitors[0].SkillID = "another-skill"
			case "different-version":
				project.SourceMonitors[0].SkillVersion = "2.0.0"
			}
			if err := store.CreateProject(t.Context(), project); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		after := ""
		for {
			page, err := store.(ProjectSkillReferenceStore).ListProjectSkillReferencesForUpgrade(t.Context(), scope, "agent-a", "forum-reader", "1.0.0", after, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, project := range page {
				if project.ID <= after {
					t.Fatalf("keyset order did not advance after %q: %q", after, project.ID)
				}
				got = append(got, project.ID)
				after = project.ID
			}
		}
		sort.Strings(matching)
		if !reflect.DeepEqual(got, matching) {
			t.Fatalf("indexed owner/assignee scope set: got=%q want=%q", got, matching)
		}
	})
}

func TestProjectSkillReferenceSQLiteMigrationBackfillsNullAndHistoricalMonitors(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "pre-reference-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateProjects(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE skill_bindings(scope_kind TEXT,scope_id TEXT,deployment_id TEXT,skill_id TEXT,skill_version TEXT,payload TEXT)`); err != nil {
		t.Fatal(err)
	}
	project := projectFixture(Scope{Kind: "tenant", ID: "pre-upgrade"})
	if err := writeProjectSkillMetadataSQL(t.Context(), db, "projects", project, true, false); err != nil {
		t.Fatal(err)
	}
	nullProject := cloneProject(project)
	nullProject.ID, nullProject.SourceMonitors = "null-history", nil
	if err := writeProjectSkillMetadataSQL(t.Context(), db, "projects", nullProject, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE projects SET payload=json_set(payload,'$.sourceMonitors',NULL) WHERE id=?`, nullProject.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrateProjectSkillReferencesSQLite(db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM project_skill_references`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("backfill count=%d err=%v", count, err)
	}
	if _, err := db.Exec(`UPDATE projects SET revision=2 WHERE id=?`, nullProject.ID); err != nil {
		t.Fatalf("explicit null historical array blocked metadata: %v", err)
	}
	if err := migrateProjectSkillReferencesSQLite(db); err != nil {
		t.Fatalf("restart migration: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM projects WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM project_skill_references`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted Project retained references count=%d err=%v", count, err)
	}
}

type projectSkillMetadataQueryRecorder struct {
	*sql.DB
	query string
	args  []interface{}
}

func (q *projectSkillMetadataQueryRecorder) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	q.query, q.args = query, append([]interface{}(nil), args...)
	return q.DB.QueryContext(ctx, query, args...)
}

func TestProjectSkillReferenceSQLitePageUsesCoveringMetadataIndexes(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "metadata-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := projectFixture(Scope{Kind: "tenant", ID: "query-plan"})
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	recorder := &projectSkillMetadataQueryRecorder{DB: store.db}
	page, err := listProjectSkillReferenceUpgradePageSQL(t.Context(), recorder, project.Scope, project.SourceMonitors[0].AssignedAgentID, "forum-reader", "1.0.0", "", 500, "projects", "project_skill_references", false)
	if err != nil || len(page) != 1 {
		t.Fatalf("metadata page=%#v err=%v", page, err)
	}
	rows, err := store.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+recorder.query, recorder.args...)
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
	plan := strings.Join(details, "\n")
	for _, index := range []string{"idx_project_skill_references_owner", "idx_project_skill_references_assigned"} {
		if !strings.Contains(plan, "USING COVERING INDEX "+index) {
			t.Fatalf("metadata seek lost covering index %s:\n%s", index, plan)
		}
	}
	pointLookup := false
	for _, detail := range details {
		if strings.HasPrefix(detail, "SEARCH p ") && strings.Contains(detail, "scope_kind=? AND scope_id=? AND id=?") {
			pointLookup = true
		}
		if strings.HasPrefix(detail, "SCAN p") {
			t.Fatalf("reference lookup scanned the Project inventory:\n%s", plan)
		}
	}
	if !pointLookup || strings.Contains(plan, "SCAN project_skill_references") {
		t.Fatalf("reference lookup scanned the whole Project inventory:\n%s", plan)
	}
}

func TestProjectSkillReferencePostgresPageUsesFullPrimaryKeyLookup(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		db, _, postgres := projectSkillMetadataSQLStore(t, fixture)
		if !postgres {
			t.Skip("PostgreSQL lookup plan contract")
		}
		store := fixture.store.(*PostgresStore)
		project := projectFixture(Scope{Kind: "tenant", ID: "query-plan"})
		if err := store.CreateProject(t.Context(), project); err != nil {
			t.Fatal(err)
		}
		// A one-row table makes a scope-only scan just as cheap as a point seek.
		// Exercise the actual inventory-growth boundary with same-scope Projects
		// owned and monitored by unrelated agents, then refresh planner statistics.
		unrelated := cloneProject(project)
		unrelated.Owner.ID = "another-owner"
		unrelated.SourceMonitors[0].AssignedAgentID = "another-agent"
		inventoryPayload, err := json.Marshal(unrelated)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO `+store.table("projects")+`(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,payload)
			SELECT 'unrelated-'||n,$1,$2,$3,$4,$5,$6,$7,jsonb_set($8::jsonb,'{id}',to_jsonb('unrelated-'||n)) FROM generate_series(1,10000) n`,
			project.Scope.Kind, project.Scope.ID, unrelated.Owner.Type, unrelated.Owner.ID, unrelated.Status, unrelated.Revision, unrelated.UpdatedAt, string(inventoryPayload)); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"projects", "project_skill_references"} {
			if _, err := db.ExecContext(t.Context(), `ANALYZE `+store.table(table)); err != nil {
				t.Fatal(err)
			}
		}
		recorder := &projectSkillMetadataQueryRecorder{DB: db}
		page, err := listProjectSkillReferenceUpgradePageSQL(t.Context(), recorder, project.Scope, project.SourceMonitors[0].AssignedAgentID, "forum-reader", "1.0.0", "", 500, store.table("projects"), store.table("project_skill_references"), true)
		if err != nil || len(page) != 1 {
			t.Fatalf("metadata page=%#v err=%v", page, err)
		}
		var payload []byte
		if err := db.QueryRowContext(t.Context(), "EXPLAIN (FORMAT JSON) "+recorder.query, recorder.args...).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		type planNode struct {
			NodeType     string     `json:"Node Type"`
			RelationName string     `json:"Relation Name"`
			IndexName    string     `json:"Index Name"`
			IndexCond    string     `json:"Index Cond"`
			Filter       string     `json:"Filter"`
			Plans        []planNode `json:"Plans"`
		}
		var explain []struct{ Plan planNode }
		if err := json.Unmarshal(payload, &explain); err != nil || len(explain) != 1 {
			t.Fatalf("decode lookup plan: %v", err)
		}
		pointLookup := false
		var inspect func(planNode)
		inspect = func(node planNode) {
			if node.RelationName == "projects" {
				if node.NodeType != "Index Scan" || node.IndexName != "projects_pkey" || !strings.Contains(node.IndexCond, "scope_kind") || !strings.Contains(node.IndexCond, "scope_id") || !strings.Contains(node.IndexCond, "(id = ") {
					t.Fatalf("Project lookup is not a full primary-key seek: %#v; query=%s; plan=%s", node, recorder.query, payload)
				}
				pointLookup = true
			}
			for _, child := range node.Plans {
				inspect(child)
			}
		}
		inspect(explain[0].Plan)
		if !pointLookup {
			t.Fatal("lookup plan omitted the canonical Project primary key")
		}
	})
}

func BenchmarkProjectSkillReferenceMetadataUnrelatedHistory(b *testing.B) {
	for _, size := range []int{100, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			store, err := NewSQLiteStore(filepath.Join(b.TempDir(), "metadata-benchmark.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			project := projectFixture(Scope{Kind: "tenant", ID: "metadata-benchmark"})
			if err := store.CreateProject(context.Background(), project); err != nil {
				b.Fatal(err)
			}
			tx, err := store.db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < size; i++ {
				unrelated := cloneProject(project)
				unrelated.ID, unrelated.Owner.ID = fmt.Sprintf("unrelated-%05d", i), "another-owner"
				unrelated.SourceMonitors[0].AssignedAgentID = "another-agent"
				if err := writeProjectSkillMetadataSQL(context.Background(), tx, "projects", unrelated, true, false); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			b.Run("page", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					page, err := store.ListProjectSkillReferencesForUpgrade(context.Background(), project.Scope, project.SourceMonitors[0].AssignedAgentID, "forum-reader", "1.0.0", "", 500)
					if err != nil || len(page) != 1 {
						b.Fatalf("metadata page=%#v err=%v", page, err)
					}
				}
			})
			b.Run("completeness", func(b *testing.B) {
				application := &SkillReferenceUpgradeMutation{Plan: &SkillReferenceUpgradePlan{Scope: project.Scope, DeploymentID: project.SourceMonitors[0].AssignedAgentID, From: SkillReferenceIdentity{ID: "forum-reader", Version: "1.0.0"}, Projects: []SkillReferenceProjectImpact{{ID: project.ID, ExpectedRevision: project.Revision, MonitorIDs: []string{project.SourceMonitors[0].ID}}}}}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := validateProjectSkillUpgradeReferencesSQL(context.Background(), store.db, application, "project_skill_references", false); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
