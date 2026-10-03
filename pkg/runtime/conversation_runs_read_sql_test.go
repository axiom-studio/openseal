package runtime

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func conversationReadSQLRun(id string, scope Scope, owner ObjectiveOwner, kind RunKind, status AgentRunStatus, at time.Time) *AgentRun {
	run := &AgentRun{ID: id, Scope: scope, Owner: owner, Kind: kind, RootRunID: id, AssignedAgentID: owner.ID,
		Goal: "Conversation read fixture", Source: RunSourceChat, Status: status, Priority: 10, AvailableAt: at, QueueEnteredAt: at,
		Revision: 1, CreatedAt: at, UpdatedAt: at}
	if isTerminalAgentRunStatus(status) {
		run.CompletedAt = &at
	}
	return run
}

func seedConversationReadSQLHistory(t *testing.T, store conversationTaskSQLTestStore, scope Scope, owner ObjectiveOwner, conversationID string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	run := conversationReadSQLRun("history", scope, owner, RunKindConversation, AgentRunStatusCompleted, now)
	run.Context = map[string]interface{}{conversationRunContextConversationID: conversationID}
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
	seed := func(insert func(*AgentRun) error) {
		for i := 0; i < 10000; i++ {
			run.ID = fmt.Sprintf("unrelated-root-%05d", i)
			run.RootRunID = run.ID
			if i%2 == 0 {
				run.Context[conversationRunContextConversationID] = conversationID
			} else {
				run.Context[conversationRunContextConversationID] = "unrelated-conversation"
			}
			if err := insert(run); err != nil {
				t.Fatal(err)
			}
		}
	}
	switch store := store.(type) {
	case *SQLiteStore:
		conn, err := beginImmediateSQLite(t.Context(), store.db)
		if err != nil {
			t.Fatal(err)
		}
		committed := false
		defer rollbackSQLiteConn(conn, &committed)
		seed(func(run *AgentRun) error { return insertSQLiteAgentRunConn(t.Context(), conn, run) })
		if _, err := conn.ExecContext(t.Context(), "COMMIT"); err != nil {
			t.Fatal(err)
		}
		committed = true
	case *PostgresStore:
		tx, err := store.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		seed(func(run *AgentRun) error { return store.insertPostgresAgentRunTx(t.Context(), tx, run) })
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unsupported SQL history fixture store")
	}
}

func TestConversationActiveRunsSQLFindsOldRootAndTracksCanonicalMembership(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		reader := fixture.store.(ConversationActiveRunsReadStore)
		scope := Scope{Kind: "tenant", ID: "legacy-read"}
		owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "legacy-agent"}
		old := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
		root := conversationReadSQLRun("old-root", scope, owner, RunKindConversation, AgentRunStatusCompleted, old)
		root.Context = map[string]interface{}{conversationRunContextConversationID: "legacy-conversation"}
		if err := fixture.store.CreateAgentRun(t.Context(), root); err != nil {
			t.Fatal(err)
		}
		children := make([]*AgentRun, 0, 2)
		for _, id := range []string{"old-child-a", "old-child-b"} {
			child := conversationReadSQLRun(id, scope, owner, RunKindAgentWork, AgentRunStatusQueued, old)
			child.RootRunID, child.ParentRunID = root.ID, root.ID
			if err := fixture.store.CreateAgentRun(t.Context(), child); err != nil {
				t.Fatal(err)
			}
			children = append(children, child)
		}
		seedConversationReadSQLHistory(t, fixture.store, scope, owner, "legacy-conversation")
		assertIDs := func(conversationID string, want ...string) {
			t.Helper()
			runs, err := reader.ListConversationActiveRuns(t.Context(), scope, owner, conversationID, 100)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(runs))
			for _, run := range runs {
				ids = append(ids, run.ID)
			}
			if !reflect.DeepEqual(ids, append([]string{}, want...)) {
				t.Fatalf("active child IDs = %v, want %v", ids, want)
			}
		}
		assertIDs("legacy-conversation", "old-child-b", "old-child-a")
		page, err := reader.ListConversationActiveRuns(t.Context(), scope, owner, "legacy-conversation", 1)
		if err != nil || len(page) != 1 || page[0].ID != "old-child-b" {
			t.Fatalf("bounded active page = %#v, %v", page, err)
		}
		for _, unrelated := range []*AgentRun{
			conversationReadSQLRun("independent-task", scope, owner, RunKindAgentWork, AgentRunStatusQueued, old),
			conversationReadSQLRun("foreign-owner-child", scope, ObjectiveOwner{Type: OwnerTypeAgent, ID: "other-agent"}, RunKindAgentWork, AgentRunStatusQueued, old),
			conversationReadSQLRun("foreign-scope-child", Scope{Kind: scope.Kind, ID: "other-tenant"}, owner, RunKindAgentWork, AgentRunStatusQueued, old),
			conversationReadSQLRun("conversation-child", scope, owner, RunKindConversation, AgentRunStatusQueued, old),
		} {
			unrelated.Context = map[string]interface{}{conversationRunContextConversationID: "legacy-conversation"}
			if unrelated.ID != "independent-task" {
				unrelated.RootRunID, unrelated.ParentRunID = root.ID, root.ID
			}
			if err := fixture.store.CreateAgentRun(t.Context(), unrelated); err != nil {
				t.Fatal(err)
			}
		}
		assertIDs("legacy-conversation", "old-child-b", "old-child-a")
		for _, query := range []struct {
			scope Scope
			owner ObjectiveOwner
		}{{Scope{Kind: scope.Kind, ID: "other-tenant"}, owner}, {scope, ObjectiveOwner{Type: OwnerTypeAgent, ID: "other-agent"}}} {
			runs, err := reader.ListConversationActiveRuns(t.Context(), query.scope, query.owner, "legacy-conversation", 100)
			if err != nil || len(runs) != 0 {
				t.Fatalf("active child lookup crossed scope or owner: %#v, %v", runs, err)
			}
		}
		root.Context[conversationRunContextConversationID] = "moved-conversation"
		root.Revision++
		updateConversationTaskSQLRun(t, fixture.store, root, root.Revision-1)
		assertIDs("legacy-conversation")
		assertIDs("moved-conversation", "old-child-b", "old-child-a")
		root.Checkpoint = map[string]interface{}{"progress": "ordinary root checkpoint"}
		root.Revision++
		updateConversationTaskSQLRun(t, fixture.store, root, root.Revision-1)
		assertIDs("moved-conversation", "old-child-b", "old-child-a")
		children[1].Status = AgentRunStatusPaused
		children[1].Revision++
		updateConversationTaskSQLRun(t, fixture.store, children[1], children[1].Revision-1)
		assertIDs("moved-conversation", "old-child-b", "old-child-a")
		completedAt := time.Now().UTC()
		children[1].Status, children[1].CompletedAt, children[1].Revision = AgentRunStatusCompleted, &completedAt, children[1].Revision+1
		updateConversationTaskSQLRun(t, fixture.store, children[1], children[1].Revision-1)
		assertIDs("moved-conversation", "old-child-a")
		root.Owner.ID = "other-agent"
		root.Revision++
		updateConversationTaskSQLRun(t, fixture.store, root, root.Revision-1)
		assertIDs("moved-conversation")
		root.Owner = owner
		root.Revision++
		updateConversationTaskSQLRun(t, fixture.store, root, root.Revision-1)
		assertIDs("moved-conversation", "old-child-a")
		if _, err := reader.ListConversationActiveRuns(t.Context(), scope, owner, "moved-conversation", 101); err == nil {
			t.Fatal("unbounded active child query accepted")
		}
	})
}

func TestConversationForegroundRunsSQLIncludesExactLegacyAndThreadRoots(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		reader := fixture.store.(ConversationForegroundRunsReadStore)
		scope := Scope{Kind: "tenant", ID: "foreground-read"}
		owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "foreground-agent"}
		at := time.Now().UTC().Truncate(time.Microsecond)
		for _, testCase := range []struct {
			id           string
			key          string
			conversation string
			thread       string
			kind         RunKind
		}{
			{"legacy-root", "conversation", "", "", RunKindConversation},
			{"thread-root", "conversation:thread:thread-message", "conversation", "thread-message", RunKindConversation},
			{"bad-thread", "conversation:thread:other-message", "conversation", "thread-message", RunKindConversation},
			{"context-only", "unrelated", "conversation", "thread-message", RunKindConversation},
			{"missing-thread", "conversation:thread:thread-message", "conversation", "", RunKindConversation},
			{"work-root", "conversation", "conversation", "", RunKindAgentWork},
		} {
			run := conversationReadSQLRun(testCase.id, scope, owner, testCase.kind, AgentRunStatusCompleted, at)
			run.ConcurrencyKey = testCase.key
			run.Context = map[string]interface{}{conversationRunContextConversationID: testCase.conversation, "threadRootMessageId": testCase.thread}
			if err := fixture.store.CreateAgentRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}
		runs, err := reader.ListConversationForegroundRuns(t.Context(), scope, owner, "conversation", 1, 0)
		if err != nil || len(runs) != 1 || runs[0].ID != "thread-root" {
			t.Fatalf("first foreground page = %#v, %v", runs, err)
		}
		runs, err = reader.ListConversationForegroundRuns(t.Context(), scope, owner, "conversation", 1, 1)
		if err != nil || len(runs) != 1 || runs[0].ID != "legacy-root" {
			t.Fatalf("second foreground page = %#v, %v", runs, err)
		}
		runs, err = reader.ListConversationForegroundRuns(t.Context(), scope, owner, "conversation", 500, 2)
		if err != nil || len(runs) != 0 {
			t.Fatalf("foreground roots admitted a false concurrency match: %#v, %v", runs, err)
		}
		for _, query := range []struct {
			scope Scope
			owner ObjectiveOwner
		}{{Scope{Kind: scope.Kind, ID: "other-tenant"}, owner}, {scope, ObjectiveOwner{Type: OwnerTypeAgent, ID: "other-agent"}}} {
			runs, err := reader.ListConversationForegroundRuns(context.Background(), query.scope, query.owner, "conversation", 100, 0)
			if err != nil || len(runs) != 0 {
				t.Fatalf("foreground query crossed scope or owner: %#v, %v", runs, err)
			}
		}
		if _, err := reader.ListConversationForegroundRuns(t.Context(), scope, owner, "conversation", 501, 0); err == nil {
			t.Fatal("unbounded foreground query accepted")
		}
		if _, err := reader.ListConversationForegroundRuns(t.Context(), scope, owner, "conversation", 10, -1); err == nil {
			t.Fatal("negative foreground offset accepted")
		}
	})
}
