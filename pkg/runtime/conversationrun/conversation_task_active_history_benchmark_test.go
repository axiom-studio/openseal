package conversationrun

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// This benchmark uses production admission and canonical Run mutation during
// untimed fixture construction. The timed path calls the production Store API.
// Five OLD active tasks sit behind NEWER completed history so LIMIT cannot hide
// a history scan. After measurement they are completed to measure the empty
// active page in the same conversation with the same retained history.
func BenchmarkConversationTaskSQLiteActiveHistory(b *testing.B) {
	conversationTaskBenchmarkActiveHistory(b, func(b *testing.B) (storetest.ConversationTaskStore, conversationTaskBenchmarkExplainPlans) {
		path := filepath.Join(b.TempDir(), "tasks.db")
		store, err := sqlite.New(path)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = store.Close() })
		db := kerneltest.OpenSQLiteDB(b, path)
		return store, func(b *testing.B, filter models.ConversationTaskFilter, _ int) {
			conversationTaskBenchmarkExplain(b, db, filter)
			conversationActiveRunsBenchmarkExplain(b, db, filter)
		}
	})
}

func BenchmarkConversationTaskPostgresActiveHistory(b *testing.B) {
	dsn := os.Getenv("OPENSEAL_TASK_BENCH_POSTGRES_DSN")
	if dsn == "" {
		b.Skip("set OPENSEAL_TASK_BENCH_POSTGRES_DSN to an isolated benchmark database")
	}
	conversationTaskBenchmarkActiveHistory(b, func(b *testing.B) (storetest.ConversationTaskStore, conversationTaskBenchmarkExplainPlans) {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			b.Fatal(err)
		}
		schema := storetest.PostgresSchema{DSN: dsn, Name: fmt.Sprintf("conversation_task_benchmark_%x", time.Now().UnixNano()), DB: db}
		store, err := postgres.New(context.Background(), dsn, postgres.WithSchema(schema.Name))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema.Quoted()+" CASCADE"); err != nil {
				b.Error(err)
			}
			_ = store.Close()
			_ = db.Close()
		})
		var config string
		if err := db.QueryRowContext(context.Background(), "SELECT current_setting('server_version') || ' fsync=' || current_setting('fsync') || ' synchronous_commit=' || current_setting('synchronous_commit')").Scan(&config); err != nil {
			b.Fatal(err)
		}
		b.Logf("isolated PostgreSQL: %s", config)
		return store, func(b *testing.B, filter models.ConversationTaskFilter, history int) {
			conversationTaskPostgresBenchmarkExplain(b, schema, filter, history)
		}
	})
}

// conversationTaskBenchmarkExplainPlans checks the backend's plans for the
// task page of filter over history completed tasks, on the benchmark's own
// connection to the store's database.
type conversationTaskBenchmarkExplainPlans func(b *testing.B, filter models.ConversationTaskFilter, history int)

func conversationTaskBenchmarkActiveHistory(b *testing.B, open func(*testing.B) (storetest.ConversationTaskStore, conversationTaskBenchmarkExplainPlans)) {
	for _, history := range []int{100, 10000} {
		b.Run(fmt.Sprintf("completed_%d", history), func(b *testing.B) {
			store, explain := open(b)
			base := time.Now().UTC().Add(-2 * time.Hour)
			kernel, ok := store.(models.ConversationTaskKernelStore)
			if !ok {
				b.Fatal("benchmark requires canonical task kernel store")
			}
			conversation := &models.Conversation{ID: "conversation", Scope: models.Scope{Kind: "tenant", ID: "task-sql"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "task-agent"}, Title: "Task performance fixture", Status: models.ConversationStatusActive, Revision: 1, CreatedAt: base.Add(-time.Minute), UpdatedAt: base.Add(-time.Minute)}
			if _, _, err := kernel.CreateConversation(context.Background(), conversation, "benchmark-conversation"); err != nil {
				b.Fatal(err)
			}
			conversationTaskBenchmarkPost(b, kernel, "thread", "", base.Add(-time.Second))
			active := make([]*models.AgentRun, 0, 5)
			for index := 0; index < 5; index++ {
				record := conversationTaskBenchmarkRecord(b, store, fmt.Sprintf("old-active-%d", index), base.Add(time.Duration(index)*time.Nanosecond))
				conversationTaskBenchmarkPost(b, kernel, record.Task.SourceMessageID, "thread", record.Task.CreatedAt)
				turn := &models.AgentTurn{ID: record.Task.SourceTurnID, Scope: record.Task.Scope, RunID: record.Task.SourceRunID, Sequence: 1, Status: models.AgentTurnStatusCompleted, Revision: 1, CreatedAt: record.Task.CreatedAt, UpdatedAt: record.Task.CreatedAt, StartedAt: record.Task.CreatedAt, CompletedAt: &record.Task.CreatedAt}
				if _, err := kernel.CreateAgentTurn(context.Background(), turn); err != nil {
					b.Fatal(err)
				}
				service := NewConversationTaskService(kernel, models.WithClock(func() time.Time { return record.Task.CreatedAt }))
				result, err := service.Start(context.Background(), StartConversationTaskRequest{Scope: record.Task.Scope, SourceRunID: record.Task.SourceRunID, ExpectedSourceRevision: 1, WorkerID: "task-worker", TurnID: turn.ID, AssignedAgentID: record.Task.TargetAgentID, TaskKey: record.Task.TaskKey, Goal: record.Task.Goal, Acknowledgment: record.Task.Acknowledgment, Budget: models.CloneBudgetPolicy(record.Task.RequestedBudget)})
				if err != nil {
					b.Fatal(err)
				}
				active = append(active, result.WorkRun)
			}
			for index := 0; index < history; index++ {
				record := conversationTaskBenchmarkRecord(b, store, fmt.Sprintf("new-completed-%d", index), base.Add(time.Hour+time.Duration(index)*time.Nanosecond))
				result, err := store.CreateConversationTask(context.Background(), record)
				if err != nil {
					b.Fatal(err)
				}
				storetest.CompleteRun(b, store, result.WorkRun)
			}
			snapshotRecord := conversationTaskBenchmarkRecord(b, store, "snapshot", time.Now().UTC())
			conversationTaskBenchmarkPost(b, kernel, snapshotRecord.Task.SourceMessageID, "thread", snapshotRecord.Task.CreatedAt)
			snapshotSource, err := store.GetAgentRun(context.Background(), snapshotRecord.Task.Scope, snapshotRecord.Task.SourceRunID)
			if err != nil || snapshotSource == nil {
				b.Fatalf("snapshot source: %v", err)
			}
			filter := models.ConversationTaskFilter{
				Scope: models.Scope{Kind: "tenant", ID: "task-sql"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "task-agent"},
				ConversationID: "conversation", ThreadRootID: "thread", AuthenticatedActor: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"},
				ActiveOnly: true, Limit: 20,
			}
			// Print the actual scoped indexed SQL plan once per fixture. This
			// is untimed and complements timings with an asymptotic invariant.
			explain(b, filter, history)
			b.Run("five_old_active", func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					page, err := store.ListConversationTasks(context.Background(), filter)
					if err != nil || len(page) != 5 {
						b.Fatalf("page=%d err=%v", len(page), err)
					}
				}
			})
			runner := &TurnRunner{portfolio: store}
			trigger := &models.ChannelMessage{ID: "current-status-message", Scope: filter.Scope, ConversationID: filter.ConversationID, Sender: filter.AuthenticatedActor, ThreadRootID: filter.ThreadRootID}
			b.Run("foreground_active_context", func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					page, err := runner.activeConversationRuns(context.Background(), conversation, trigger)
					if err != nil || len(page) != 5 {
						b.Fatalf("active context=%d err=%v", len(page), err)
					}
				}
			})
			b.Run("hosted_active_snapshot", func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					snapshot, err := ResolveCatalogConversationTaskContext(context.Background(), nil, snapshotSource, kernel)
					if err != nil || snapshot == nil || !snapshot.CanStart || len(snapshot.Tasks) != 5 {
						b.Fatalf("active snapshot=%#v err=%v", snapshot, err)
					}
				}
			})
			b.Run("absent_conversation", func(b *testing.B) {
				empty := filter
				empty.ConversationID = "absent-conversation"
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					page, err := store.ListConversationTasks(context.Background(), empty)
					if err != nil || len(page) != 0 {
						b.Fatalf("page=%d err=%v", len(page), err)
					}
				}
			})
			for _, run := range active {
				storetest.CompleteRun(b, store, run)
			}
			b.Run("foreground_idle_context", func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					page, err := runner.activeConversationRuns(context.Background(), conversation, trigger)
					if err != nil || len(page) != 0 {
						b.Fatalf("idle context=%d err=%v", len(page), err)
					}
				}
			})
			b.Run("hosted_idle_snapshot", func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					snapshot, err := ResolveCatalogConversationTaskContext(context.Background(), nil, snapshotSource, kernel)
					if err != nil || snapshot == nil || !snapshot.CanStart || len(snapshot.Tasks) != 0 {
						b.Fatalf("idle snapshot=%#v err=%v", snapshot, err)
					}
				}
			})
			b.Run("no_active_same_history", func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					page, err := store.ListConversationTasks(context.Background(), filter)
					if err != nil || len(page) != 0 {
						b.Fatalf("page=%d err=%v", len(page), err)
					}
				}
			})
		})
	}
}

func conversationTaskBenchmarkExplain(t testing.TB, db *sql.DB, filter models.ConversationTaskFilter) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT t.payload, r.payload FROM conversation_tasks t
        JOIN agent_runs r ON r.scope_kind=t.scope_kind AND r.scope_id=t.scope_id AND r.id=t.work_run_id
        WHERE t.scope_kind=? AND t.scope_id=? AND t.conversation_id=? AND t.owner_type=? AND t.owner_id=?
        AND t.actor_type=? AND t.actor_id=? AND t.thread_root_id=? AND t.is_active=1
        ORDER BY t.created_at DESC,t.id DESC LIMIT ?`, filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.Owner.Type, filter.Owner.ID, filter.AuthenticatedActor.Type, filter.AuthenticatedActor.ID, filter.ThreadRootID, filter.Limit)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	activeIndex := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Logf("query plan: %s", detail)
		if strings.Contains(detail, "idx_conversation_tasks_active_") {
			activeIndex = true
		}
		if strings.Contains(detail, "SCAN ") || strings.Contains(detail, "USE TEMP B-TREE") {
			t.Fatalf("unbounded active page plan: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !activeIndex {
		t.Fatal("active task page index was not used")
	}
}

func conversationTaskBenchmarkRecord(t testing.TB, store storetest.ConversationTaskStore, suffix string, at time.Time) models.ConversationTaskCreateRecord {
	t.Helper()
	now := at
	expires := time.Now().UTC().Add(10 * time.Minute)
	scope := models.Scope{Kind: "tenant", ID: "task-sql"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "task-agent"}
	sourceID := "source-" + suffix
	source := &models.AgentRun{ID: sourceID, Scope: scope, Kind: models.RunKindConversation, RootRunID: sourceID, Owner: owner,
		AssignedAgentID: owner.ID, ConcurrencyKey: models.ConversationRunLane("conversation", "thread", owner.ID), Goal: "Accept conversation work", Source: models.RunSourceChat, Status: models.AgentRunStatusRunning,
		Priority: 90, AvailableAt: now, QueueEnteredAt: now, LeaseOwner: "task-worker", LeaseExpiresAt: &expires,
		LastAppliedTurn: 1, Budget: &models.BudgetPolicy{MaxTurns: 20}, Policy: map[string]interface{}{"preserve": "source-policy"},
		Context:  map[string]interface{}{models.ConversationRunConversationIDContextKey: "conversation", models.ConversationRunTriggerIDContextKey: "message-" + suffix, "threadRootMessageId": "thread"},
		Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateAgentRun(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	taskID := models.ConversationTaskID(scope, sourceID, "task-"+suffix)
	workID := "work-" + suffix
	// The task asks for the child minimum of 8 Turns.
	task := &models.ConversationTask{ID: taskID, Scope: scope, Owner: owner, ConversationID: "conversation", SourceMessageID: "message-" + suffix,
		ThreadRootID: "thread", SourceRunID: sourceID, SourceTurnID: "turn-" + suffix, SourceTurnNumber: 1,
		AuthenticatedActor: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"}, TargetAgentID: owner.ID,
		WorkRunID: workID, TaskKey: "task-" + suffix, Goal: "Prepare the requested result", Acknowledgment: "I have started the work.",
		RequestedBudget: &models.BudgetPolicy{MaxTurns: 8}, Revision: 1, CreatedAt: now}
	var err error
	task.RequestDigest, err = models.ConversationTaskDigest(task)
	if err != nil {
		t.Fatal(err)
	}
	work := &models.AgentRun{ID: workID, Scope: scope, Kind: models.RunKindAgentWork, RootRunID: workID, Owner: owner, AssignedAgentID: owner.ID,
		ConcurrencyKey: "task:" + taskID, Goal: task.Goal, Source: models.RunSourceChat, Status: models.AgentRunStatusQueued, Priority: source.Priority,
		AvailableAt: now, QueueEnteredAt: now, Budget: models.CloneBudgetPolicy(task.RequestedBudget), Revision: 1, CreatedAt: now, UpdatedAt: now,
		Context: map[string]interface{}{models.ConversationTaskContextKey: taskID, models.ConversationRunConversationIDContextKey: task.ConversationID,
			models.ConversationRunTriggerIDContextKey: task.SourceMessageID, "threadRootMessageId": task.ThreadRootID, models.RunReportingRootRunIDContextKey: workID,
			models.RunReportingMilestonesContextKey: []interface{}{"completed", "failed"}}}
	updated := models.CloneAgentRun(source)
	if err := models.AddRunBudgetAllocation(updated, workID, work.Budget); err != nil {
		t.Fatal(err)
	}
	updated.Status = models.AgentRunStatusCompleted
	updated.Context[models.RunReportingRootRunIDContextKey] = source.ID
	updated.Context[models.RunReportingMilestonesContextKey] = []interface{}{"completed", "failed"}
	updated.Output = map[string]interface{}{"summary": task.Acknowledgment, "conversationTaskIds": []string{taskID}, "conversationTaskWorkRunId": workID}
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Revision++
	event := &models.ActivityEvent{ID: "task-event-" + suffix, Scope: scope, RunID: sourceID, EventType: "conversation.task_started",
		Summary: "Started independent work", Actor: models.ActivityActor{Type: "user", ID: "requester"}, CorrelationID: taskID,
		Visibility: models.ActivityVisibilityScope, CreatedAt: now}
	record := models.ConversationTaskCreateRecord{Task: task, WorkRun: work, SourceRun: updated, ExpectedSourceRevision: 1, WorkerID: "task-worker", Event: event}
	if err := models.ValidateConversationTaskCreateRecord(record); err != nil {
		t.Fatalf("invalid SQL fixture: %v", err)
	}
	return record
}

func conversationActiveRunsBenchmarkExplain(t testing.TB, db *sql.DB, filter models.ConversationTaskFilter) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT child.payload FROM conversation_active_runs linked
		JOIN agent_runs child INDEXED BY sqlite_autoindex_agent_runs_1 ON child.scope_kind=linked.scope_kind AND child.scope_id=linked.scope_id AND child.id=linked.id
		JOIN agent_runs root ON root.scope_kind=linked.scope_kind AND root.scope_id=linked.scope_id AND root.id=linked.root_run_id
		WHERE linked.scope_kind=? AND linked.scope_id=? AND linked.owner_type=? AND linked.owner_id=? AND linked.conversation_id=?
		AND child.root_run_id=root.id AND child.id<>root.id
		AND json_extract(root.payload, '$.owner.type')=linked.owner_type AND json_extract(root.payload, '$.owner.id')=linked.owner_id
		AND json_extract(root.payload, '$.context.conversationId')=linked.conversation_id AND json_extract(root.payload, '$.kind')='conversation'
		AND json_extract(child.payload, '$.owner.type')=linked.owner_type AND json_extract(child.payload, '$.owner.id')=linked.owner_id
		AND COALESCE(json_extract(child.payload, '$.kind'), 'agent_work')<>'conversation' AND child.status NOT IN ('completed','failed','canceled')
		ORDER BY linked.created_at DESC,linked.id DESC LIMIT ?`, filter.Scope.Kind, filter.Scope.ID, filter.Owner.Type, filter.Owner.ID, filter.ConversationID, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	activeIndex := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Logf("active child query plan: %s", detail)
		if strings.Contains(detail, "idx_conversation_active_runs_page") {
			activeIndex = true
		}
		if strings.Contains(detail, "SCAN ") || strings.Contains(detail, "USE TEMP B-TREE") {
			t.Fatalf("unbounded active child page plan: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !activeIndex {
		t.Fatal("active conversation page index was not used")
	}
}

func conversationTaskPostgresBenchmarkExplain(t testing.TB, schema storetest.PostgresSchema, filter models.ConversationTaskFilter, history int) {
	t.Helper()
	// This is an isolated fixture: refreshing its planner statistics performs no
	// work against platform/application tables and is excluded from timings.
	for _, table := range []string{"conversation_tasks", "agent_runs", "conversation_active_runs"} {
		if _, err := schema.DB.ExecContext(context.Background(), "ANALYZE "+schema.Table(table)); err != nil {
			t.Fatal(err)
		}
	}
	queries := []struct {
		name string
		sql  string
		args []interface{}
	}{
		{"task", `SELECT t.payload,r.payload FROM ` + schema.Table("conversation_tasks") + ` t
		JOIN ` + schema.Table("agent_runs") + ` r ON r.scope_kind=t.scope_kind AND r.scope_id=t.scope_id AND r.id=t.work_run_id
		WHERE t.scope_kind=$1 AND t.scope_id=$2 AND t.conversation_id=$3 AND t.owner_type=$4 AND t.owner_id=$5
		AND t.actor_type=$6 AND t.actor_id=$7 AND t.thread_root_id=$8 AND t.is_active
		ORDER BY t.created_at DESC,t.id DESC LIMIT $9`, []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.Owner.Type, filter.Owner.ID, filter.AuthenticatedActor.Type, filter.AuthenticatedActor.ID, filter.ThreadRootID, filter.Limit}},
		{"active_child", `WITH linked AS MATERIALIZED (
		SELECT * FROM ` + schema.Table("conversation_active_runs") + `
		WHERE scope_kind=$1 AND scope_id=$2 AND owner_type=$3 AND owner_id=$4 AND conversation_id=$5
		ORDER BY created_at DESC,id DESC LIMIT $6
		)
		SELECT child.payload FROM linked
		CROSS JOIN LATERAL (SELECT payload,id,root_run_id,status FROM ` + schema.Table("agent_runs") + `
		WHERE scope_kind=linked.scope_kind AND scope_id=linked.scope_id AND id=linked.id OFFSET 0) child
		CROSS JOIN LATERAL (SELECT payload,id FROM ` + schema.Table("agent_runs") + `
		WHERE scope_kind=linked.scope_kind AND scope_id=linked.scope_id AND id=linked.root_run_id OFFSET 0) root
		WHERE child.root_run_id=root.id AND child.id<>root.id AND root.payload->>'kind'='conversation'
		AND root.payload->'owner'->>'type'=linked.owner_type AND root.payload->'owner'->>'id'=linked.owner_id
		AND root.payload->'context'->>'conversationId'=linked.conversation_id
		AND child.payload->'owner'->>'type'=linked.owner_type AND child.payload->'owner'->>'id'=linked.owner_id
		AND COALESCE(child.payload->>'kind','agent_work')<>'conversation' AND child.status NOT IN ('completed','failed','canceled')
		ORDER BY linked.created_at DESC,linked.id DESC LIMIT $6`, []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.Owner.Type, filter.Owner.ID, filter.ConversationID, 100}},
	}
	for _, query := range queries {
		var raw []byte
		if err := schema.DB.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query.sql, query.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		t.Logf("Postgres %s plan: %s", query.name, raw)
		if query.name == "task" && history >= 10000 {
			var plans []struct{ Plan map[string]interface{} }
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			activeIndex := false
			var visit func(map[string]interface{})
			visit = func(node map[string]interface{}) {
				index, _ := node["Index Name"].(string)
				if strings.HasPrefix(index, "conversation_tasks_active_") {
					activeIndex = true
				}
				if node["Node Type"] == "Seq Scan" && node["Relation Name"] == "conversation_tasks" {
					t.Fatal("task page sequentially scanned completed history")
				}
				if children, ok := node["Plans"].([]interface{}); ok {
					for _, child := range children {
						if entry, ok := child.(map[string]interface{}); ok {
							visit(entry)
						}
					}
				}
			}
			for _, plan := range plans {
				visit(plan.Plan)
			}
			if !activeIndex {
				t.Fatal("10k-history task page did not use an active partial index")
			}
		}
	}
}

func conversationTaskBenchmarkPost(t testing.TB, store models.ConversationTaskKernelStore, id, reply string, at time.Time) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "task-sql"}
	conversation, err := store.GetConversation(context.Background(), scope, "conversation")
	if err != nil || conversation == nil {
		t.Fatalf("benchmark conversation: %v", err)
	}
	service := runtimeconversation.NewService(store, models.WithClock(func() time.Time { return at }))
	_, err = service.PostChannelMessage(context.Background(), runtimeconversation.PostChannelMessageRequest{ID: id, Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"}, Intent: models.MessageIntentQuestion, Content: "Review the requested result", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: reply, RequiresResponse: true, IdempotencyKey: "benchmark-message-" + id})
	if err != nil {
		t.Fatal(err)
	}
}
