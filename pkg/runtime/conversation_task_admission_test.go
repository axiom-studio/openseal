package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
)

type conversationTaskAdmissionTestStore interface {
	conversationTaskSQLTestStore
	ConversationStore
	AgentRunScheduleStore
	AgentRunAdmissionStore
}

func forConversationTaskAdmissionStores(t *testing.T, test func(*testing.T, conversationTaskAdmissionTestStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemoryStore()) })
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		test(t, fixture.store.(conversationTaskAdmissionTestStore))
	})
}

// Unlike the low-level task transaction fixture, an admission fixture includes
// the authoritative channel message from which the task's actor was derived.
func conversationTaskAdmissionRecord(t *testing.T, store conversationTaskAdmissionTestStore, suffix string) ConversationTaskCreateRecord {
	t.Helper()
	record := conversationTaskSQLRecord(t, store, suffix)
	task := record.Task
	conversation, err := store.GetConversation(t.Context(), task.Scope, task.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conversation == nil {
		conversation, _, err = store.CreateConversation(t.Context(), &Conversation{
			ID: task.ConversationID, Scope: task.Scope, Owner: task.Owner, Title: "Task admission",
			Status: ConversationStatusActive, Revision: 1, CreatedAt: task.CreatedAt, UpdatedAt: task.CreatedAt,
		}, "task-admission-chat")
		if err != nil {
			t.Fatal(err)
		}
	}
	message := &ChannelMessage{ID: task.SourceMessageID, Scope: task.Scope, ConversationID: task.ConversationID,
		Sequence: conversation.LastSequence + 1, Sender: task.AuthenticatedActor, Intent: MessageIntentQuestion,
		Content: "Prepare the requested background result", ThreadRootID: task.ThreadRootID,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: task.SourceMessageID, CreatedAt: task.CreatedAt}
	next := cloneConversation(conversation)
	next.Revision++
	next.LastSequence = message.Sequence
	if _, err := store.CommitChannelMessage(t.Context(), ChannelMessageCommitRecord{Conversation: next, ExpectedRevision: conversation.Revision, Message: message}); err != nil {
		t.Fatal(err)
	}
	return record
}

func createConversationAdmissionTask(t *testing.T, store conversationTaskAdmissionTestStore, suffix string) *ConversationTaskResult {
	t.Helper()
	result, err := store.CreateConversationTask(t.Context(), conversationTaskAdmissionRecord(t, store, suffix))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func conversationTaskAdmissionClaim(now time.Time, reserve int) AgentRunClaim {
	return AgentRunClaim{Scope: Scope{Kind: "tenant", ID: "task-sql"}, Kind: RunKindAgentWork,
		WorkerID: "task-admission-worker", AssignedAgentID: "task-agent", Now: now,
		LeaseDuration: 5 * time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4,
		ConversationTaskForegroundReserve: reserve}
}

func assertTaskAdmissionBlocked(t *testing.T, decision *AgentRunAdmissionDecision, id string, reason AgentRunAdmissionReason, limit int) {
	t.Helper()
	if decision == nil || decision.Run != nil || decision.Outcome != AgentRunAdmissionBackpressured {
		t.Fatalf("expected backpressure, got %#v", decision)
	}
	for _, block := range decision.Blocks {
		if block.RunID == id && block.Reason == reason && block.Limit == limit {
			return
		}
	}
	t.Fatalf("missing block for %s with %s/%d: %#v", id, reason, limit, decision.Blocks)
}

func TestConversationTaskForegroundReserveLeavesFourthSlotForConversation(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		now := time.Now().UTC().Add(time.Second)
		tasks := make([]*ConversationTaskResult, 4)
		for i := range tasks {
			tasks[i] = createConversationAdmissionTask(t, store, fmt.Sprintf("reserve-%d", i))
		}
		scheduler := NewAgentRunScheduler(store)
		scheduler.now = func() time.Time { return now }
		request := AgentRunClaimRequest{Scope: tasks[0].Task.Scope, Kind: RunKindAgentWork, WorkerID: "worker",
			AssignedAgentID: tasks[0].Task.TargetAgentID, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: 1}
		for i := 0; i < 3; i++ {
			decision, err := scheduler.ClaimNextDecision(t.Context(), request)
			if err != nil || decision.Run == nil || decision.Run.Kind != RunKindAgentWork {
				t.Fatalf("claim %d: %#v %v", i, decision, err)
			}
		}
		decision, err := scheduler.ClaimNextDecision(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, tasks[3].WorkRun.ID, AgentRunAdmissionReasonTaskCapacity, 3)
		inspected, err := scheduler.Inspect(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, inspected, tasks[3].WorkRun.ID, AgentRunAdmissionReasonTaskCapacity, 3)
		foreground := cloneAgentRun(tasks[0].SourceRun)
		foreground.ID, foreground.RootRunID, foreground.Status = "foreground", "foreground", AgentRunStatusQueued
		foreground.Revision, foreground.Output, foreground.CompletedAt = 1, nil, nil
		if err := store.CreateAgentRun(t.Context(), foreground); err != nil {
			t.Fatal(err)
		}
		claim := conversationTaskAdmissionClaim(now, 0)
		claim.Kind = RunKindConversation
		claimed, err := store.ClaimNextAgentRun(t.Context(), claim)
		if err != nil || claimed == nil || claimed.ID != foreground.ID {
			t.Fatalf("foreground fourth slot: %#v %v", claimed, err)
		}
		// The reserve does not bypass the existing ceiling when that slot is held.
		claim.ConversationTaskForegroundReserve = 1
		claim.Kind = RunKindAgentWork
		decision, err = store.ClaimNextAgentRunWithDecision(t.Context(), claim)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, tasks[3].WorkRun.ID, AgentRunAdmissionReasonAgentCapacity, 4)
	})
}

func TestConversationTaskForegroundReserveSerializesConcurrentClaims(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for i := 0; i < 8; i++ {
			createConversationAdmissionTask(t, store, fmt.Sprintf("concurrent-%d", i))
		}
		var wg sync.WaitGroup
		claimed := make(chan *AgentRun, 8)
		errorsFound := make(chan error, 8)
		now := time.Now().UTC().Add(time.Second)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				claim := conversationTaskAdmissionClaim(now, 1)
				claim.WorkerID = fmt.Sprintf("worker-%d", i)
				run, err := store.ClaimNextAgentRun(t.Context(), claim)
				if err != nil {
					errorsFound <- err
				} else if run != nil {
					claimed <- run
				}
			}(i)
		}
		wg.Wait()
		close(claimed)
		close(errorsFound)
		for err := range errorsFound {
			t.Error(err)
		}
		if len(claimed) != 3 {
			t.Fatalf("concurrent claims = %d, want 3", len(claimed))
		}
	})
}

func TestConversationTaskForegroundReserveAllowsThirdTaskAfterForeground(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		tasks := make([]*ConversationTaskResult, 3)
		for i := range tasks {
			tasks[i] = createConversationAdmissionTask(t, store, fmt.Sprintf("order-%d", i))
		}
		claim := conversationTaskAdmissionClaim(time.Now().Add(time.Second), 1)
		for i := 0; i < 2; i++ {
			run, err := store.ClaimNextAgentRun(t.Context(), claim)
			if err != nil || run == nil {
				t.Fatalf("initial task %d: %#v %v", i, run, err)
			}
		}
		foreground := cloneAgentRun(tasks[0].SourceRun)
		foreground.ID, foreground.RootRunID, foreground.Status = "foreground-first", "foreground-first", AgentRunStatusQueued
		foreground.Revision, foreground.Output, foreground.CompletedAt = 1, nil, nil
		if err := store.CreateAgentRun(t.Context(), foreground); err != nil {
			t.Fatal(err)
		}
		foregroundClaim := claim
		foregroundClaim.Kind, foregroundClaim.ConversationTaskForegroundReserve = RunKindConversation, 0
		if run, err := store.ClaimNextAgentRun(t.Context(), foregroundClaim); err != nil || run == nil || run.ID != foreground.ID {
			t.Fatalf("foreground: %#v %v", run, err)
		}
		run, err := store.ClaimNextAgentRun(t.Context(), claim)
		if err != nil || run == nil || run.ID != tasks[2].WorkRun.ID {
			t.Fatalf("third task with foreground already held: %#v %v", run, err)
		}
	})
}

func TestConversationTaskForegroundReserveCoversPersistedForks(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		task := createConversationAdmissionTask(t, store, "fork-root")
		// A waiting task parent does not count as a live claim. Its verified forks do.
		root := cloneAgentRun(task.WorkRun)
		root.Status, root.Revision = AgentRunStatusWaitingForDependency, root.Revision+1
		updateConversationTaskSQLRun(t, store, root, task.WorkRun.Revision)
		parent := root
		for i := 0; i < 4; i++ {
			child := cloneAgentRun(task.WorkRun)
			child.ID, child.ParentRunID, child.Source = fmt.Sprintf("fork-%d", i), parent.ID, RunSourceFork
			child.ConcurrencyKey, child.Context = "fork:"+child.ID, nil // Ancestry, not a copied hint, is authoritative.
			if i < 3 {
				expires := time.Now().Add(5 * time.Minute)
				child.Status, child.LeaseOwner, child.LeaseExpiresAt = AgentRunStatusRunning, "held-worker", &expires
			}
			if err := store.CreateAgentRun(t.Context(), child); err != nil {
				t.Fatal(err)
			}
			parent = child
		}
		decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), conversationTaskAdmissionClaim(time.Now().Add(time.Second), 1))
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, "fork-3", AgentRunAdmissionReasonTaskCapacity, 3)
	})
}

func TestConversationTaskForegroundReserveRejectsForgedAuthority(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for _, scenario := range []string{"actor", "source", "hint", "cross-agent", "cross-owner", "missing-parent", "cycle"} {
			t.Run(scenario, func(t *testing.T) {
				record := conversationTaskAdmissionRecord(t, store, "forged-"+scenario)
				if scenario == "actor" {
					record.Task.AuthenticatedActor.ID = "other-human"
					record.Task.RequestDigest, _ = conversationTaskDigest(record.Task)
				}
				result, err := store.CreateConversationTask(t.Context(), record)
				if err != nil {
					t.Fatal(err)
				}
				candidate := cloneAgentRun(result.WorkRun)
				if scenario == "source" {
					candidate.Context[conversationRunContextTriggerID] = "other-source"
					candidate.Revision++
					updateConversationTaskSQLRun(t, store, candidate, result.WorkRun.Revision)
				} else if scenario != "actor" {
					root := cloneAgentRun(result.WorkRun)
					root.Status, root.Revision = AgentRunStatusWaitingForDependency, root.Revision+1
					updateConversationTaskSQLRun(t, store, root, result.WorkRun.Revision)
					candidate.ID, candidate.ParentRunID, candidate.Source = "candidate-"+scenario, root.ID, RunSourceFork
					candidate.ConcurrencyKey = "fork:" + candidate.ID
					switch scenario {
					case "hint":
						candidate.RootRunID, candidate.ParentRunID = candidate.ID, ""
					case "cross-agent":
						candidate.AssignedAgentID = "other-agent"
					case "cross-owner":
						candidate.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "other-agent"}
					case "missing-parent":
						candidate.ParentRunID = "missing"
					case "cycle":
						candidate.ParentRunID = candidate.ID
					}
					if err := store.CreateAgentRun(t.Context(), candidate); err != nil {
						t.Fatal(err)
					}
				}
				claim := conversationTaskAdmissionClaim(time.Now().Add(time.Second), 1)
				claim.AssignedAgentID = candidate.AssignedAgentID
				decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
				if err != nil {
					t.Fatal(err)
				}
				assertTaskAdmissionBlocked(t, decision, candidate.ID, AgentRunAdmissionReasonTaskProvenance, 0)
			})
		}
	})
}

func TestConversationTaskForegroundReservePreservesOrdinaryAndDefaultPolicy(t *testing.T) {
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for i := 0; i < 4; i++ {
			task := createConversationAdmissionTask(t, store, fmt.Sprintf("default-%d", i))
			claim := conversationTaskAdmissionClaim(time.Now().Add(time.Second), 0)
			run, err := store.ClaimNextAgentRun(t.Context(), claim)
			if err != nil || run == nil || run.ID != task.WorkRun.ID {
				t.Fatalf("zero reserve claim %d: %#v %v", i, run, err)
			}
		}
	})
	forConversationTaskAdmissionStores(t, func(t *testing.T, store conversationTaskAdmissionTestStore) {
		for i := 0; i < 5; i++ {
			now := time.Now().Add(-time.Minute)
			run := &AgentRun{ID: fmt.Sprintf("ordinary-%d", i), RootRunID: fmt.Sprintf("ordinary-%d", i),
				Scope: Scope{Kind: "tenant", ID: "task-sql"}, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "task-agent"},
				AssignedAgentID: "task-agent", Source: RunSourceSchedule, Goal: "Ordinary scheduled work", Status: AgentRunStatusQueued,
				AvailableAt: now, QueueEnteredAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
			if err := store.CreateAgentRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}
		claim := conversationTaskAdmissionClaim(time.Now(), 1)
		for i := 0; i < 4; i++ {
			run, err := store.ClaimNextAgentRun(t.Context(), claim)
			if err != nil || run == nil {
				t.Fatalf("ordinary claim %d: %#v %v", i, run, err)
			}
		}
		decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskAdmissionBlocked(t, decision, "ordinary-4", AgentRunAdmissionReasonAgentCapacity, 4)
	})
}

func TestConversationTaskForegroundReserveZeroDoesNotReadTaskProof(t *testing.T) {
	claim := conversationTaskAdmissionClaim(time.Now(), 0)
	lookup := conversationTaskAdmissionLookup{task: func(context.Context, Scope, string) (*ConversationTask, error) { panic("zero reserve read task proof") }}
	if err := prepareConversationTaskAdmission(t.Context(), &claim, []*AgentRun{{ID: "plain", Scope: claim.Scope, Status: AgentRunStatusQueued}}, lookup); err != nil {
		t.Fatal(err)
	}
	for _, reserve := range []int{-1, 4, 5} {
		claim.ConversationTaskForegroundReserve = reserve
		if err := claim.Validate(); err == nil {
			t.Fatalf("invalid reserve %d accepted", reserve)
		}
		worker := AgentRunWorkerConfig{Scope: claim.Scope, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: reserve}
		if err := worker.applyDefaults(); err == nil {
			t.Fatalf("worker invalid reserve %d accepted", reserve)
		}
		dynamic := DynamicAgentRunWorkerConfig{MaxActiveForAgent: 4, ConversationTaskForegroundReserve: reserve}
		if err := dynamic.applyDefaults(); err == nil {
			t.Fatalf("supervisor invalid reserve %d accepted", reserve)
		}
	}
}

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
