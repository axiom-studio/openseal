package runtime

import (
	"database/sql"
	"testing"
	"time"
)

// Postgres store test: it checks the store's own migration ledger, so it moves
// to the Postgres store package with the store.

func TestConversationTaskSQLPostgresMigrationRollbackAndReapply(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		store, ok := fixture.store.(*PostgresStore)
		if !ok {
			t.Skip("PostgreSQL migration ledger and rollback")
		}
		current, version, err := store.postgresSchemaCurrent(t.Context(), store.db)
		if err != nil || !current || version != currentPostgresSchemaVersion {
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
		if err != nil || !current || version != currentPostgresSchemaVersion {
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
