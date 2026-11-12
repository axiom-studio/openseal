package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

// Postgres store test: it reads the plan of the store's own admission query,
// so it moves to the Postgres store package with the store.

func TestConversationTaskForegroundReservePostgresIndexedAdmissionPlan(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		store, ok := fixture.store.(*PostgresStore)
		if !ok {
			t.Skip("PostgreSQL query-plan regression")
		}
		task := createConversationAdmissionTask(t, store, "plan")
		// Existing runnable indexes must reject old completed work and future
		// queued work before loading their JSON payloads or proving task lineage.
		_, err := store.db.ExecContext(t.Context(), `INSERT INTO `+store.table("agent_runs")+`
			(scope_kind,scope_id,id,objective_id,parent_run_id,root_run_id,assigned_agent_id,status,priority,revision,
			 available_at,deadline,queue_entered_at,lease_owner,lease_expires_at,last_claimed_at,attempt,created_at,payload)
			SELECT scope_kind,scope_id,'plan-history-'||n,objective_id,parent_run_id,'plan-history-'||n,assigned_agent_id,
			 CASE WHEN n <= 2000 THEN 'completed' ELSE 'queued' END,priority,revision,
			 CASE WHEN n <= 2000 THEN available_at ELSE available_at + INTERVAL '10 years' END,
			 deadline,queue_entered_at,lease_owner,NULL,last_claimed_at,attempt,created_at,
			 jsonb_set(jsonb_set(jsonb_set(payload,'{id}',to_jsonb('plan-history-'||n)),'{rootRunId}',to_jsonb('plan-history-'||n)),
			 '{status}',to_jsonb(CASE WHEN n <= 2000 THEN 'completed'::text ELSE 'queued'::text END))
			FROM `+store.table("agent_runs")+` CROSS JOIN generate_series(1,4000) AS n
			WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, task.Task.Scope.Kind, task.Task.Scope.ID, task.WorkRun.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(t.Context(), `ANALYZE `+store.table("agent_runs")); err != nil {
			t.Fatal(err)
		}
		claim := conversationTaskAdmissionClaim(time.Now().Add(time.Second), 1)
		var planJSON string
		err = store.db.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+store.agentRunAdmissionSQL(store.conversationTaskAdmissionSQL()),
			claim.Scope.Kind, claim.Scope.ID, claim.AssignedAgentID, AgentRunStatusQueued, AgentRunStatusRunning,
			claim.Now, claim.MaxActiveForAgent, claim.AgingInterval.Seconds(), claim.Kind, claim.MaxActiveForConcurrencyKey,
			claim.MaxActiveForOwner, claim.MaxActiveForObjective, claim.MaxActiveForAgent-claim.ConversationTaskForegroundReserve, pq.Array([]string{})).Scan(&planJSON)
		if err != nil {
			t.Fatal(err)
		}
		var plans []struct {
			Plan map[string]interface{} `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(planJSON), &plans); err != nil || len(plans) != 1 {
			t.Fatalf("decode explain: %v", err)
		}
		var walk func(map[string]interface{})
		walk = func(node map[string]interface{}) {
			if node["Relation Name"] == "agent_runs" {
				if node["Node Type"] == "Seq Scan" {
					t.Fatalf("admission scanned queued/history payloads: %s", planJSON)
				}
				if removed, ok := node["Rows Removed by Filter"].(float64); ok && removed > 32 {
					t.Fatalf("admission filtered history after fetching it: %s", planJSON)
				}
			}
			if children, ok := node["Plans"].([]interface{}); ok {
				for _, child := range children {
					walk(child.(map[string]interface{}))
				}
			}
		}
		walk(plans[0].Plan)
		if !strings.Contains(planJSON, "agent_runs_runnable_idx") {
			t.Fatalf("runnable index absent: %s", planJSON)
		}
		t.Logf("indexed reserve admission EXPLAIN: %s", planJSON)
	})
}
