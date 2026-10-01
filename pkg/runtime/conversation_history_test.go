package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConversationHistoryStores(t *testing.T) {
	for _, name := range []string{"memory", "sqlite", "postgres"} {
		t.Run(name, func(t *testing.T) {
			var store interface {
				KernelStore
				ActionStore
			}
			switch name {
			case "memory":
				store = NewMemoryStore()
			case "sqlite":
				s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "history.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				store = s
			case "postgres":
				dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated PostgreSQL")
				}
				s, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(fmt.Sprintf("chat_history_%d", time.Now().UnixNano())))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = s.db.ExecContext(context.Background(), `DROP SCHEMA `+s.quotedSchema()+` CASCADE`)
					_ = s.Close()
				})
				store = s
			}
			testConversationHistory(t, store)
		})
	}
}

func testConversationHistory(t *testing.T, store interface {
	KernelStore
	ActionStore
}) {
	scope := Scope{Kind: "tenant", ID: "one"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	portfolio := NewPortfolioService(store)
	create := func(scope Scope, owner ObjectiveOwner, conversation, parent string) *AgentRun {
		t.Helper()
		context := map[string]interface{}{}
		if conversation != "" {
			context["conversationId"] = conversation
			context["triggerMessageId"] = "message-" + conversation
		}
		run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: "Review", Source: RunSourceManual, ParentRunID: parent, Context: context})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	root := create(scope, owner, "selected", "")
	child := create(scope, ObjectiveOwner{Type: OwnerTypeAgent, ID: "delegate"}, "", root.ID)
	unrelated := create(scope, owner, "other", "")
	legacy := create(scope, owner, "", "")
	foreign := create(Scope{Kind: "tenant", ID: "two"}, owner, "selected", "")
	for i, run := range []*AgentRun{root, child, unrelated, legacy, foreign} {
		proposal := sqliteApprovalProposal(run, fmt.Sprintf("history-call-%d", i), fmt.Sprintf("history-key-%d", i), fmt.Sprintf("history-event-%d", i))
		if _, err := store.CreateActionProposal(t.Context(), proposal); err != nil {
			t.Fatal(err)
		}
	}
	approvals, err := store.ListApprovals(t.Context(), ApprovalFilter{Scope: scope, Owner: &owner, ConversationID: "selected", Limit: 100})
	if err != nil || len(approvals) != 2 {
		t.Fatalf("scoped approvals: %d, %v", len(approvals), err)
	}
	for _, approval := range approvals {
		if approval.ConversationContext == nil || approval.ConversationContext.ConversationID != "selected" || approval.ConversationContext.TriggerMessageID != "message-selected" {
			t.Fatalf("missing inherited context: %#v", approval)
		}
	}
	approvals, err = store.ListApprovals(t.Context(), ApprovalFilter{Scope: scope, Owner: &owner, ConversationID: "selected", IncludeUnscoped: true, Limit: 100})
	if err != nil || len(approvals) != 3 {
		t.Fatalf("legacy approvals: %d, %v", len(approvals), err)
	}
	// Projection is response-only; a list must not mutate the durable checkpoint.
	original, err := store.GetApproval(t.Context(), scope, "approval-history-call-0")
	if err != nil || original.ConversationContext != nil {
		t.Fatalf("projection persisted: %#v, %v", original, err)
	}
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ConversationID: "selected", IncludeDescendants: true, Limit: 100})
	if err != nil || len(runs) != 2 {
		t.Fatalf("scoped run lineage: %d, %v", len(runs), err)
	}
	exact, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ConversationID: "selected", Limit: 100})
	if err != nil || len(exact) != 1 || exact[0].ID != root.ID {
		t.Fatalf("exact runs: %#v, %v", exact, err)
	}
	page, err := store.ListApprovals(t.Context(), ApprovalFilter{Scope: scope, Owner: &owner, ConversationID: "selected", Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("scoped paging: %d, %v", len(page), err)
	}
}
