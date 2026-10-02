package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestLegacyRunSkillDependencyRequiresProvableImmutableMethod(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "legacy-method"}
	deployment, definition := acceptedMethodFixture(scope)
	original := &AgentRun{ID: "legacy", Scope: scope, Status: AgentRunStatusPaused, AssignedAgentID: "agent", Entrypoint: "manual", Context: map[string]interface{}{"runbookDefinitionId": "reader-method", "runbookDefinitionVersion": "1"}}
	for _, test := range []struct {
		name   string
		mutate func(*AgentRun, *kernelagent.AgentDeployment, *kernelagent.AgentDefinition)
		want   bool
	}{
		{name: "exact context", want: true},
		{name: "exact plan", want: true, mutate: func(r *AgentRun, _ *kernelagent.AgentDeployment, _ *kernelagent.AgentDefinition) {
			r.Context = nil
			r.Plan = map[string]interface{}{"runbook": map[string]interface{}{"id": "reader-method", "version": "1"}}
		}},
		{name: "unknown historical method", mutate: func(r *AgentRun, _ *kernelagent.AgentDeployment, _ *kernelagent.AgentDefinition) { r.Context = nil }},
		{name: "stale context", mutate: func(r *AgentRun, _ *kernelagent.AgentDeployment, _ *kernelagent.AgentDefinition) {
			r.Context["runbookDefinitionVersion"] = "0"
		}},
		{name: "conflicting plan", mutate: func(r *AgentRun, _ *kernelagent.AgentDeployment, _ *kernelagent.AgentDefinition) {
			r.Plan = map[string]interface{}{"runbook": map[string]interface{}{"id": "reader-method", "version": "2"}}
		}},
		{name: "foreign tenant", mutate: func(_ *AgentRun, d *kernelagent.AgentDeployment, _ *kernelagent.AgentDefinition) {
			d.Scope.ID = "foreign"
		}},
		{name: "wrong immutable definition", mutate: func(_ *AgentRun, _ *kernelagent.AgentDeployment, d *kernelagent.AgentDefinition) { d.Version = "2" }},
		{name: "terminal", mutate: func(r *AgentRun, _ *kernelagent.AgentDeployment, _ *kernelagent.AgentDefinition) {
			r.Status = AgentRunStatusCompleted
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := cloneAgentRun(original)
			d, f := *deployment, *definition
			if test.mutate != nil {
				test.mutate(run, &d, &f)
			}
			before := cloneAgentRun(run)
			pin := legacyAcceptedRunExecution(run, &d, &f)
			if (pin != nil) != test.want {
				t.Fatalf("legacy proof = %#v, wanted valid=%v", pin, test.want)
			}
			if pin != nil && !reflect.DeepEqual(pin.SkillDependencies, []SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}}) {
				t.Fatalf("wrong exact dependency: %#v", pin)
			}
			if !reflect.DeepEqual(run, before) {
				t.Fatal("legacy dependency proof rewrote historical Run")
			}
		})
	}
}

func TestLegacyRunSkillDependencyBackfillPagesWithoutRewritingHistoryAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		var db *sql.DB
		var runs, deployments, definitions, dependencies string
		postgres := false
		switch store := fixture.store.(type) {
		case *SQLiteStore:
			db, runs, deployments, definitions, dependencies = store.db, "agent_runs", "agent_deployments", "agent_definitions", "run_skill_dependencies"
		case *PostgresStore:
			db, runs, deployments, definitions, dependencies = store.db, store.table("agent_runs"), store.table("agent_deployments"), store.table("agent_definitions"), store.table("run_skill_dependencies")
			postgres = true
		default:
			t.Skip("one-time migration applies to durable stores")
		}
		store := fixture.store.(KernelStore)
		scope := Scope{Kind: "tenant", ID: "legacy-backfill"}
		deployment, definition := acceptedMethodFixture(scope)
		if err := store.(kernelagent.Store).CreateDefinition(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
		if err := store.(kernelagent.Store).CreateDeployment(t.Context(), deployment, kernelagent.DefinitionActivation{ID: "legacy-activation", Scope: skill.ScopeReference(scope), DeploymentID: deployment.ID, DeploymentRevision: 1, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		insert := `INSERT INTO ` + runs + `(scope_kind,scope_id,id,root_run_id,assigned_agent_id,status,created_at,payload) VALUES(?,?,?,?,?,?,?,?)`
		if postgres {
			insert = `INSERT INTO ` + runs + `(scope_kind,scope_id,id,root_run_id,assigned_agent_id,status,created_at,available_at,queue_entered_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$7,$7,$8::jsonb)`
		}
		count := legacyRunSkillDependencyBatch + 7
		for i := 0; i < count; i++ {
			run := &AgentRun{ID: fmt.Sprintf("legacy-%04d", i), RootRunID: fmt.Sprintf("legacy-%04d", i), Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Status: AgentRunStatusPaused, Entrypoint: "manual", Context: map[string]interface{}{"runbookDefinitionId": "reader-method", "runbookDefinitionVersion": "1"}}
			payload, _ := json.Marshal(run)
			if _, err := tx.ExecContext(t.Context(), insert, scope.Kind, scope.ID, run.ID, run.ID, run.AssignedAgentID, run.Status, time.Now().UTC(), string(payload)); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range []struct {
			id      string
			scope   Scope
			version string
			status  AgentRunStatus
		}{
			{id: "unknown", scope: scope, status: AgentRunStatusPaused},
			{id: "stale", scope: scope, version: "0", status: AgentRunStatusPaused},
			{id: "terminal", scope: scope, version: "1", status: AgentRunStatusCompleted},
			{id: "foreign", scope: Scope{Kind: "tenant", ID: "foreign"}, version: "1", status: AgentRunStatusPaused},
		} {
			run := &AgentRun{ID: test.id, RootRunID: test.id, Scope: test.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Status: test.status, Entrypoint: "manual"}
			if test.version != "" {
				run.Context = map[string]interface{}{"runbookDefinitionId": "reader-method", "runbookDefinitionVersion": test.version}
			}
			payload, _ := json.Marshal(run)
			if _, err := tx.ExecContext(t.Context(), insert, test.scope.Kind, test.scope.ID, run.ID, run.ID, run.AssignedAgentID, run.Status, time.Now().UTC(), string(payload)); err != nil {
				t.Fatal(err)
			}
		}
		var before string
		if err := tx.QueryRowContext(t.Context(), `SELECT payload FROM `+runs+` WHERE id='legacy-0000'`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := backfillLegacyRunSkillDependencies(t.Context(), tx, runs, deployments, definitions, dependencies, postgres); err != nil {
			t.Fatal(err)
		}
		var projected int
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+dependencies).Scan(&projected); err != nil || projected != count {
			t.Fatalf("keyset backfill projected %d, wanted %d: %v", projected, count, err)
		}
		var after string
		if err := tx.QueryRowContext(t.Context(), `SELECT payload FROM `+runs+` WHERE id='legacy-0000'`).Scan(&after); err != nil || after != before {
			t.Fatalf("legacy historical Run was rewritten: %v", err)
		}
		if !postgres {
			rows, err := tx.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT scope_kind,scope_id,id FROM agent_runs WHERE status IN (`+unfinishedSkillRunSQL+`) AND (scope_kind,scope_id,id)>('tenant','legacy-backfill','legacy-0000') ORDER BY scope_kind,scope_id,id LIMIT 1000`)
			if err != nil {
				t.Fatal(err)
			}
			plan := ""
			for rows.Next() {
				var a, b, c int
				var line string
				if err := rows.Scan(&a, &b, &c, &line); err != nil {
					t.Fatal(err)
				}
				plan += line
			}
			rows.Close()
			if !strings.Contains(plan, "idx_agent_runs_legacy_skill_dependency_page") || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("legacy page is not index-bounded: %s", plan)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0"}); err != nil || !busy {
			t.Fatalf("legacy metadata was not retained: %v %v", busy, err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(KernelStore)
		}
		if refs, err := store.(SkillRuntimeUsageStore).ListReferencedSkillRuntimeVersions(t.Context(), scope); err != nil || !reflect.DeepEqual(refs, []SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}}) {
			t.Fatalf("restart lost legacy executable identity: %#v %v", refs, err)
		}
	})
}
