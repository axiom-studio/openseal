package runtime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type conversationTaskSQLTestStore interface {
	PortfolioStore
	RunActivityStore
	ConversationTaskStore
}

type conversationTaskSQLTestFixture struct {
	store  conversationTaskSQLTestStore
	reopen func(*testing.T) conversationTaskSQLTestStore
}

var conversationTaskSQLFixtureTime = time.Now().UTC().Truncate(time.Microsecond)

func forConversationTaskSQLStores(t *testing.T, test func(*testing.T, conversationTaskSQLTestFixture)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conversation-tasks.db")
		open := func(t *testing.T) conversationTaskSQLTestStore {
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
		test(t, conversationTaskSQLTestFixture{store: open(t), reopen: open})
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL conversation task regressions")
		}
		schema := "openseal_task_sql_" + uuid.NewString()[:8]
		open := func(t *testing.T) conversationTaskSQLTestStore {
			store, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
		store := open(t)
		t.Cleanup(func() {
			cleanup, err := sql.Open("postgres", dsn)
			if err != nil {
				return
			}
			defer cleanup.Close()
			_, _ = cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.(*PostgresStore).quotedSchema()+` CASCADE`)
		})
		test(t, conversationTaskSQLTestFixture{store: store, reopen: open})
	})
}

func conversationTaskSQLRecord(t *testing.T, store conversationTaskSQLTestStore, suffix string) ConversationTaskCreateRecord {
	t.Helper()
	now := conversationTaskSQLFixtureTime
	expires := time.Now().UTC().Add(10 * time.Minute)
	scope := Scope{Kind: "tenant", ID: "task-sql"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "task-agent"}
	sourceID := "source-" + suffix
	source := &AgentRun{ID: sourceID, Scope: scope, Kind: RunKindConversation, RootRunID: sourceID, Owner: owner,
		AssignedAgentID: owner.ID, Goal: "Accept conversation work", Source: RunSourceChat, Status: AgentRunStatusRunning,
		Priority: 90, AvailableAt: now, QueueEnteredAt: now, LeaseOwner: "task-worker", LeaseExpiresAt: &expires,
		LastAppliedTurn: 1, Budget: &BudgetPolicy{MaxTurns: 20}, Policy: map[string]interface{}{"preserve": "source-policy"},
		Context:  map[string]interface{}{conversationRunContextConversationID: "conversation", conversationRunContextTriggerID: "message-" + suffix},
		Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateAgentRun(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	taskID := conversationTaskID(scope, sourceID, "task-"+suffix)
	workID := "work-" + suffix
	task := &ConversationTask{ID: taskID, Scope: scope, Owner: owner, ConversationID: "conversation", SourceMessageID: "message-" + suffix,
		ThreadRootID: "thread", SourceRunID: sourceID, SourceTurnID: "turn-" + suffix, SourceTurnNumber: 1,
		AuthenticatedActor: ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}, TargetAgentID: owner.ID,
		WorkRunID: workID, TaskKey: "task-" + suffix, Goal: "Prepare the requested result", Acknowledgment: "I have started the work.",
		RequestedBudget: &BudgetPolicy{MaxTurns: 5}, Revision: 1, CreatedAt: now}
	var err error
	task.RequestDigest, err = conversationTaskDigest(task)
	if err != nil {
		t.Fatal(err)
	}
	work := &AgentRun{ID: workID, Scope: scope, Kind: RunKindAgentWork, RootRunID: workID, Owner: owner, AssignedAgentID: owner.ID,
		ConcurrencyKey: "task:" + taskID, Goal: task.Goal, Source: RunSourceChat, Status: AgentRunStatusQueued, Priority: source.Priority,
		AvailableAt: now, QueueEnteredAt: now, Budget: cloneBudgetPolicy(task.RequestedBudget), Revision: 1, CreatedAt: now, UpdatedAt: now,
		Context: map[string]interface{}{ConversationTaskContextKey: taskID, conversationRunContextConversationID: task.ConversationID,
			conversationRunContextTriggerID: task.SourceMessageID, runReportingContextRootRunID: workID,
			runReportingContextMilestones: []interface{}{"completed", "failed"}}}
	updated := cloneAgentRun(source)
	if err := addRunBudgetAllocation(updated, workID, work.Budget); err != nil {
		t.Fatal(err)
	}
	updated.Status = AgentRunStatusCompleted
	updated.Context[runReportingContextRootRunID] = source.ID
	updated.Context[runReportingContextMilestones] = []interface{}{"completed", "failed"}
	updated.Output = map[string]interface{}{"summary": task.Acknowledgment, "conversationTaskIds": []string{taskID}, "conversationTaskWorkRunId": workID}
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Revision++
	event := &ActivityEvent{ID: "task-event-" + suffix, Scope: scope, RunID: sourceID, EventType: "conversation.task_started",
		Summary: "Started independent work", Actor: ActivityActor{Type: "user", ID: "requester"}, CorrelationID: taskID,
		Visibility: ActivityVisibilityScope, CreatedAt: now}
	record := ConversationTaskCreateRecord{Task: task, WorkRun: work, SourceRun: updated, ExpectedSourceRevision: 1, WorkerID: "task-worker", Event: event}
	if err := validateConversationTaskCreateRecord(record); err != nil {
		t.Fatalf("invalid SQL fixture: %v", err)
	}
	return record
}

func updateConversationTaskSQLRun(t *testing.T, store conversationTaskSQLTestStore, run *AgentRun, expectedRevision int64) {
	t.Helper()
	event := &ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.updated",
		Summary: "Update SQL task test run", Actor: ActivityActor{Type: "user", ID: "requester"}, Visibility: ActivityVisibilityScope, CreatedAt: run.UpdatedAt}
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, expectedRevision, event, nil); err != nil {
		t.Fatal(err)
	}
}

func TestConversationTaskSQLConcurrentReplayAndRestart(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		record := conversationTaskSQLRecord(t, fixture.store, "replay")
		second := fixture.reopen(t)
		var wg sync.WaitGroup
		results := make(chan *ConversationTaskResult, 8)
		errorsFound := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				store := fixture.store
				if i%2 == 1 {
					store = second
				}
				result, err := store.CreateConversationTask(t.Context(), record)
				if err != nil {
					errorsFound <- err
					return
				}
				results <- result
			}(i)
		}
		wg.Wait()
		close(errorsFound)
		close(results)
		for err := range errorsFound {
			t.Errorf("concurrent admission: %v", err)
		}
		created := 0
		for result := range results {
			if result.Task.ID != record.Task.ID || result.WorkRun.ID != record.WorkRun.ID || result.SourceRun.Status != AgentRunStatusCompleted {
				t.Fatalf("concurrent result lost task identity: %#v", result)
			}
			if !result.Replayed {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("fresh admissions = %d, want 1", created)
		}
		if err := fixture.store.(interface{ Close() error }).Close(); err != nil {
			t.Fatal(err)
		}
		restarted := fixture.reopen(t)
		replay, err := restarted.CreateConversationTask(t.Context(), record)
		if err != nil || replay == nil || !replay.Replayed || replay.SourceRun.Revision != 2 {
			t.Fatalf("restart replay = %#v, %v", replay, err)
		}
		if replay.WorkRun.ParentRunID != "" || replay.WorkRun.RootRunID != replay.WorkRun.ID || replay.SourceRun.BudgetAllocations[replay.WorkRun.ID].MaxTurns != 5 {
			t.Fatalf("independent work or budget allocation lost: %#v", replay)
		}
		events, err := restarted.ListActivity(t.Context(), ActivityFilter{Scope: record.Task.Scope, RunID: record.Task.SourceRunID, Limit: 100})
		if err != nil || len(events) != 1 {
			t.Fatalf("audit duplicated across retries: events=%#v err=%v", events, err)
		}
		conflicting := record
		conflicting.Task = cloneConversationTask(record.Task)
		conflicting.Task.Goal = "Different requested result"
		conflicting.Task.RequestDigest, _ = conversationTaskDigest(conflicting.Task)
		conflicting.WorkRun = cloneAgentRun(record.WorkRun)
		conflicting.WorkRun.Goal = conflicting.Task.Goal
		if _, err := restarted.CreateConversationTask(t.Context(), conflicting); !errors.Is(err, ErrConversationTaskConflict) {
			t.Fatalf("changed accepted request error = %v", err)
		}
		foreign := Scope{Kind: record.Task.Scope.Kind, ID: "other-tenant"}
		if task, err := restarted.GetConversationTask(t.Context(), foreign, record.Task.ID); err != nil || task != nil {
			t.Fatalf("cross-scope task lookup = %#v, %v", task, err)
		}
		if task, err := restarted.FindConversationTaskByWorkRunID(t.Context(), foreign, record.WorkRun.ID); err != nil || task != nil {
			t.Fatalf("cross-scope work lookup = %#v, %v", task, err)
		}
	})
}

func TestConversationTaskSQLRejectsSourceMutationAndRollsBackAuditFailure(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		for _, scenario := range []string{"worker", "expiry", "prepared-expiry", "revision", "context", "budget", "audit"} {
			t.Run(scenario, func(t *testing.T) {
				record := conversationTaskSQLRecord(t, fixture.store, scenario)
				current, err := fixture.store.GetAgentRun(t.Context(), record.Task.Scope, record.Task.SourceRunID)
				if err != nil {
					t.Fatal(err)
				}
				want := ErrConversationTaskConflict
				switch scenario {
				case "worker":
					record.WorkerID = "foreign-worker"
					want = ErrLeaseLost
				case "expiry", "prepared-expiry", "revision":
					changed := cloneAgentRun(current)
					changed.Revision++
					if scenario == "expiry" || scenario == "prepared-expiry" {
						changed.LeaseExpiresAt = &record.Task.CreatedAt
						if scenario == "prepared-expiry" {
							preparedAt := record.Task.CreatedAt.Add(-time.Hour)
							expiredAt := time.Now().UTC().Add(-time.Second)
							record.Task.CreatedAt = preparedAt
							changed.LeaseExpiresAt = &expiredAt
						}
						record.ExpectedSourceRevision++
						record.SourceRun.Revision++
						want = ErrLeaseLost
					} else {
						want = ErrRevisionConflict
					}
					updateConversationTaskSQLRun(t, fixture.store, changed, current.Revision)
					current = changed
				case "context":
					record.SourceRun.Context["unrelated"] = "must be preserved"
				case "budget":
					record.SourceRun.BudgetAllocations = nil
				case "audit":
					if _, err := fixture.store.AppendActivity(t.Context(), record.Event); err != nil {
						t.Fatal(err)
					}
				}
				_, err = fixture.store.CreateConversationTask(t.Context(), record)
				if scenario == "audit" {
					if err == nil {
						t.Fatal("duplicate audit ID admitted task")
					}
				} else if !errors.Is(err, want) {
					t.Fatalf("admission error = %v, want %v", err, want)
				}
				after, err := fixture.store.GetAgentRun(t.Context(), current.Scope, current.ID)
				if err != nil || !reflect.DeepEqual(current, after) {
					t.Fatalf("failed admission changed source: before=%#v after=%#v err=%v", current, after, err)
				}
				if task, err := fixture.store.GetConversationTask(t.Context(), record.Task.Scope, record.Task.ID); err != nil || task != nil {
					t.Fatalf("failed admission persisted task: %#v, %v", task, err)
				}
				work, err := fixture.store.GetAgentRun(t.Context(), record.Task.Scope, record.WorkRun.ID)
				if err != nil || work != nil {
					t.Fatalf("failed admission persisted work: %#v, %v", work, err)
				}
			})
		}
	})
}

func TestConversationTaskSQLScopedPaginationAndCurrentWorkStatus(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		records := make([]ConversationTaskCreateRecord, 0, 3)
		for _, suffix := range []string{"page-a", "page-b", "page-c"} {
			record := conversationTaskSQLRecord(t, fixture.store, suffix)
			if suffix == "page-c" {
				record.Task.ThreadRootID = "other-thread"
				record.Task.RequestDigest, _ = conversationTaskDigest(record.Task)
			}
			if _, err := fixture.store.CreateConversationTask(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			records = append(records, record)
		}
		filter := ConversationTaskFilter{Scope: records[0].Task.Scope, Owner: records[0].Task.Owner, ConversationID: "conversation",
			AuthenticatedActor: records[0].Task.AuthenticatedActor, Limit: 2}
		page, err := fixture.store.ListConversationTasks(t.Context(), filter)
		if err != nil || len(page) != 2 {
			t.Fatalf("first page = %#v, %v", page, err)
		}
		ids := []string{records[0].Task.ID, records[1].Task.ID, records[2].Task.ID}
		sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		if page[0].Task.ID != ids[0] || page[1].Task.ID != ids[1] {
			t.Fatalf("timestamp ties changed order: %#v", page)
		}
		filter.BeforeCreatedAt = &page[1].Task.CreatedAt
		filter.BeforeID = page[1].Task.ID
		page, err = fixture.store.ListConversationTasks(t.Context(), filter)
		if err != nil || len(page) != 1 || page[0].Task.ID != ids[2] {
			t.Fatalf("cursor skipped or duplicated task: %#v, %v", page, err)
		}
		filter.BeforeCreatedAt, filter.BeforeID = nil, ""
		filter.Limit = 100
		filter.ThreadRootID = "thread"
		page, err = fixture.store.ListConversationTasks(t.Context(), filter)
		if err != nil || len(page) != 2 {
			t.Fatalf("thread selector = %#v, %v", page, err)
		}
		work, err := fixture.store.GetAgentRun(t.Context(), records[0].Task.Scope, records[0].WorkRun.ID)
		if err != nil {
			t.Fatal(err)
		}
		now := work.CreatedAt.Add(time.Second)
		work.Status, work.CompletedAt, work.Revision = AgentRunStatusCompleted, &now, work.Revision+1
		updateConversationTaskSQLRun(t, fixture.store, work, work.Revision-1)
		filter.ActiveOnly = true
		page, err = fixture.store.ListConversationTasks(t.Context(), filter)
		if err != nil || len(page) != 1 || page[0].WorkRun.ID != records[1].WorkRun.ID {
			t.Fatalf("active selector ignored current independent run: %#v, %v", page, err)
		}
		for _, mutate := range []func(*ConversationTaskFilter){
			func(f *ConversationTaskFilter) { f.Scope.ID = "other-tenant" },
			func(f *ConversationTaskFilter) { f.Owner.ID = "other-agent" },
			func(f *ConversationTaskFilter) { f.ConversationID = "other-conversation" },
			func(f *ConversationTaskFilter) {
				f.AuthenticatedActor = ConversationParticipant{Type: ConversationParticipantUser, ID: "other-requester"}
			},
		} {
			other := filter
			mutate(&other)
			page, err := fixture.store.ListConversationTasks(t.Context(), other)
			if err != nil || len(page) != 0 {
				t.Fatalf("list crossed selector boundary: %#v, %v", page, err)
			}
		}
		filter.Limit = 101
		if _, err := fixture.store.ListConversationTasks(t.Context(), filter); !errors.Is(err, ErrInvalidConversationTask) {
			t.Fatalf("unbounded list error = %v", err)
		}
	})
}

func TestConversationTaskSQLPostgresMigrationRollbackAndReapply(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		store, ok := fixture.store.(*PostgresStore)
		if !ok {
			t.Skip("PostgreSQL migration ledger and rollback")
		}
		current, version, err := store.postgresSchemaCurrent(t.Context(), store.db)
		if err != nil || !current || version != conversationTaskMigrationVersion {
			t.Fatalf("fresh migration ledger = current:%v version:%d err:%v", current, version, err)
		}
		legacyScope := Scope{Kind: "tenant", ID: "migration-backfill"}
		legacyOwner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "legacy-agent"}
		legacyRoot := conversationReadSQLRun("pre-upgrade-root", legacyScope, legacyOwner, RunKindConversation, AgentRunStatusCompleted, conversationTaskSQLFixtureTime)
		legacyRoot.Context = map[string]interface{}{conversationRunContextConversationID: "pre-upgrade-conversation"}
		legacyChild := conversationReadSQLRun("pre-upgrade-child", legacyScope, legacyOwner, RunKindAgentWork, AgentRunStatusQueued, conversationTaskSQLFixtureTime)
		legacyChild.RootRunID, legacyChild.ParentRunID = legacyRoot.ID, legacyRoot.ID
		for _, run := range []*AgentRun{legacyRoot, legacyChild} {
			if err := store.CreateAgentRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.RollbackPostgresMigrations(t.Context(), conversationTaskMigrationVersion-1); err != nil {
			t.Fatal(err)
		}
		var relation sql.NullString
		if err := store.db.QueryRowContext(t.Context(), `SELECT to_regclass($1)`, store.schema+".conversation_tasks").Scan(&relation); err != nil || relation.Valid {
			t.Fatalf("rolled-back task table = %#v, %v", relation, err)
		}
		current, version, err = store.postgresSchemaCurrent(t.Context(), store.db)
		if err != nil || current || version != conversationTaskMigrationVersion-1 {
			t.Fatalf("rollback migration ledger = current:%v version:%d err:%v", current, version, err)
		}
		restarted := fixture.reopen(t).(*PostgresStore)
		current, version, err = restarted.postgresSchemaCurrent(t.Context(), restarted.db)
		if err != nil || !current || version != conversationTaskMigrationVersion {
			t.Fatalf("reapplied migration ledger = current:%v version:%d err:%v", current, version, err)
		}
		legacy, err := restarted.ListConversationActiveRuns(t.Context(), legacyScope, legacyOwner, "pre-upgrade-conversation", 100)
		if err != nil || len(legacy) != 1 || legacy[0].ID != legacyChild.ID {
			t.Fatalf("reapplied migration lost existing legacy work: %#v, %v", legacy, err)
		}
		// A new run status update exercises the recreated trigger and function.
		record := conversationTaskSQLRecord(t, restarted, "reapplied")
		if _, err := restarted.CreateConversationTask(t.Context(), record); err != nil {
			t.Fatal(err)
		}
		work, err := restarted.GetAgentRun(t.Context(), record.Task.Scope, record.WorkRun.ID)
		if err != nil {
			t.Fatal(err)
		}
		now := work.CreatedAt.Add(time.Second)
		work.Status, work.CompletedAt, work.Revision = AgentRunStatusCanceled, &now, work.Revision+1
		updateConversationTaskSQLRun(t, restarted, work, work.Revision-1)
		page, err := restarted.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: record.Task.Scope, Owner: record.Task.Owner,
			ConversationID: record.Task.ConversationID, ActiveOnly: true, Limit: 100})
		if err != nil || len(page) != 0 {
			t.Fatalf("reapplied active projection = %#v, %v", page, err)
		}
	})
}
