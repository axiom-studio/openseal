package runtime

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func seedConversationReadSQLHistory(t *testing.T, store conversationTaskSQLTestStore, scope models.Scope, owner models.ObjectiveOwner, conversationID string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	run := storetest.ConversationReadRun("history", scope, owner, models.RunKindConversation, models.AgentRunStatusCompleted, now)
	run.Context = map[string]interface{}{models.ConversationRunConversationIDContextKey: conversationID}
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
	// No store method writes Runs in bulk, so the history takes a few
	// seconds to build.
	for i := 0; i < 10000; i++ {
		run.ID = fmt.Sprintf("unrelated-root-%05d", i)
		run.RootRunID = run.ID
		if i%2 == 0 {
			run.Context[models.ConversationRunConversationIDContextKey] = conversationID
		} else {
			run.Context[models.ConversationRunConversationIDContextKey] = "unrelated-conversation"
		}
		if err := store.CreateAgentRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConversationActiveRunsSQLFindsOldRootAndTracksCanonicalMembership(t *testing.T) {
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		reader := fixture.store.(models.ConversationActiveRunsReadStore)
		scope := models.Scope{Kind: "tenant", ID: "legacy-read"}
		owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "legacy-agent"}
		old := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
		root := storetest.ConversationReadRun("old-root", scope, owner, models.RunKindConversation, models.AgentRunStatusCompleted, old)
		root.Context = map[string]interface{}{models.ConversationRunConversationIDContextKey: "legacy-conversation"}
		if err := fixture.store.CreateAgentRun(t.Context(), root); err != nil {
			t.Fatal(err)
		}
		children := make([]*models.AgentRun, 0, 2)
		for _, id := range []string{"old-child-a", "old-child-b"} {
			child := storetest.ConversationReadRun(id, scope, owner, models.RunKindAgentWork, models.AgentRunStatusQueued, old)
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
		for _, unrelated := range []*models.AgentRun{
			storetest.ConversationReadRun("independent-task", scope, owner, models.RunKindAgentWork, models.AgentRunStatusQueued, old),
			storetest.ConversationReadRun("foreign-owner-child", scope, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other-agent"}, models.RunKindAgentWork, models.AgentRunStatusQueued, old),
			storetest.ConversationReadRun("foreign-scope-child", models.Scope{Kind: scope.Kind, ID: "other-tenant"}, owner, models.RunKindAgentWork, models.AgentRunStatusQueued, old),
			storetest.ConversationReadRun("conversation-child", scope, owner, models.RunKindConversation, models.AgentRunStatusQueued, old),
		} {
			unrelated.Context = map[string]interface{}{models.ConversationRunConversationIDContextKey: "legacy-conversation"}
			if unrelated.ID != "independent-task" {
				unrelated.RootRunID, unrelated.ParentRunID = root.ID, root.ID
			}
			if err := fixture.store.CreateAgentRun(t.Context(), unrelated); err != nil {
				t.Fatal(err)
			}
		}
		assertIDs("legacy-conversation", "old-child-b", "old-child-a")
		for _, query := range []struct {
			scope models.Scope
			owner models.ObjectiveOwner
		}{{models.Scope{Kind: scope.Kind, ID: "other-tenant"}, owner}, {scope, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other-agent"}}} {
			runs, err := reader.ListConversationActiveRuns(t.Context(), query.scope, query.owner, "legacy-conversation", 100)
			if err != nil || len(runs) != 0 {
				t.Fatalf("active child lookup crossed scope or owner: %#v, %v", runs, err)
			}
		}
		root.Context[models.ConversationRunConversationIDContextKey] = "moved-conversation"
		root.Revision++
		updateConversationTaskSQLRun(t, fixture.store, root, root.Revision-1)
		assertIDs("legacy-conversation")
		assertIDs("moved-conversation", "old-child-b", "old-child-a")
		root.Checkpoint = map[string]interface{}{"progress": "ordinary root checkpoint"}
		root.Revision++
		updateConversationTaskSQLRun(t, fixture.store, root, root.Revision-1)
		assertIDs("moved-conversation", "old-child-b", "old-child-a")
		children[1].Status = models.AgentRunStatusPaused
		children[1].Revision++
		updateConversationTaskSQLRun(t, fixture.store, children[1], children[1].Revision-1)
		assertIDs("moved-conversation", "old-child-b", "old-child-a")
		completedAt := time.Now().UTC()
		children[1].Status, children[1].CompletedAt, children[1].Revision = models.AgentRunStatusCompleted, &completedAt, children[1].Revision+1
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
		reader := fixture.store.(models.ConversationForegroundRunsReadStore)
		scope := models.Scope{Kind: "tenant", ID: "foreground-read"}
		owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "foreground-agent"}
		at := time.Now().UTC().Truncate(time.Microsecond)
		for _, testCase := range []struct {
			id           string
			key          string
			conversation string
			thread       string
			kind         models.RunKind
		}{
			{"legacy-root", "conversation", "", "", models.RunKindConversation},
			{"thread-root", "conversation:thread:thread-message:agent:foreground-agent", "conversation", "thread-message", models.RunKindConversation},
			{"agent-root", "conversation:agent:foreground-agent", "conversation", "", models.RunKindConversation},
			{"bad-thread", "conversation:thread:other-message:agent:foreground-agent", "conversation", "thread-message", models.RunKindConversation},
			{"bare-thread", "conversation:thread:thread-message", "conversation", "thread-message", models.RunKindConversation},
			{"context-only", "unrelated", "conversation", "thread-message", models.RunKindConversation},
			{"missing-thread", "conversation:thread:thread-message:agent:foreground-agent", "conversation", "", models.RunKindConversation},
			{"work-root", "conversation", "conversation", "", models.RunKindAgentWork},
		} {
			run := storetest.ConversationReadRun(testCase.id, scope, owner, testCase.kind, models.AgentRunStatusCompleted, at)
			run.ConcurrencyKey = testCase.key
			run.Context = map[string]interface{}{models.ConversationRunConversationIDContextKey: testCase.conversation, "threadRootMessageId": testCase.thread}
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
		runs, err = reader.ListConversationForegroundRuns(t.Context(), scope, owner, "conversation", 1, 2)
		if err != nil || len(runs) != 1 || runs[0].ID != "agent-root" {
			t.Fatalf("third foreground page = %#v, %v", runs, err)
		}
		runs, err = reader.ListConversationForegroundRuns(t.Context(), scope, owner, "conversation", 500, 3)
		if err != nil || len(runs) != 0 {
			t.Fatalf("foreground roots admitted a false concurrency match: %#v, %v", runs, err)
		}
		for _, query := range []struct {
			scope models.Scope
			owner models.ObjectiveOwner
		}{{models.Scope{Kind: scope.Kind, ID: "other-tenant"}, owner}, {scope, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other-agent"}}} {
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
