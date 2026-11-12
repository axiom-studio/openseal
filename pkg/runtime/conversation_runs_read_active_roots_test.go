package runtime

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestConversationActiveRunsTracksCanonicalRootChanges(t *testing.T) {
	store, db := newTestSQLiteStore(t)
	scope := Scope{Kind: "tenant", ID: "active-root"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	at := time.Now()
	root := storetest.ConversationReadRun("root", scope, owner, RunKindConversation, AgentRunStatusCompleted, at)
	root.Context = map[string]interface{}{conversationRunContextConversationID: "original"}
	if err := store.CreateAgentRun(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	child := storetest.ConversationReadRun("child", scope, owner, RunKindAgentWork, AgentRunStatusQueued, at)
	child.ParentRunID = root.ID
	child.RootRunID = root.ID
	if err := store.CreateAgentRun(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	check := func(conversation string, want int) {
		t.Helper()
		runs, err := store.ListConversationActiveRuns(t.Context(), scope, owner, conversation, 10)
		if err != nil || len(runs) != want {
			t.Fatalf("canonical active roots=%#v err=%v want=%d", runs, err, want)
		}
	}
	check("original", 1)
	root.Context[conversationRunContextConversationID] = "moved"
	root.Revision++
	updateConversationTaskSQLRun(t, store, root, root.Revision-1)
	check("original", 0)
	check("moved", 1)
	root.Owner.ID = "other"
	root.Revision++
	updateConversationTaskSQLRun(t, store, root, root.Revision-1)
	check("moved", 0)
	root.Owner = owner
	root.Revision++
	updateConversationTaskSQLRun(t, store, root, root.Revision-1)
	check("moved", 1)
	child.Status = AgentRunStatusCompleted
	child.CompletedAt = &at
	child.Revision++
	updateConversationTaskSQLRun(t, store, child, child.Revision-1)
	check("moved", 0)
	if storetest.CountRows(t, db, "conversation_active_runs") != 0 {
		t.Fatal("terminal children retained active root lookup entries")
	}
}
