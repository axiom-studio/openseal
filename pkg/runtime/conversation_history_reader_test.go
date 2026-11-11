package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestConversationHistoryReadsVisibleOriginals(t *testing.T) {
	service := NewConversationService(newTestKernelStore(t))
	scope := Scope{Kind: "tenant", ID: "one"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: owner.ID}}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "history", IdempotencyKey: "history"})
	if err != nil {
		t.Fatal(err)
	}
	var longID, privateID string
	original := strings.Repeat("a", 4095) + strings.Repeat("日本語", 1000)
	for i := 0; i < 4; i++ {
		content := fmt.Sprintf("message %d", i)
		audience := ConversationAudience{Kind: ConversationAudienceChannel}
		if i == 1 {
			content = original
		}
		if i == 2 {
			audience = ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "private-user"}}}
		}
		result, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}, Intent: MessageIntentUpdate, Content: content, Audience: audience, IdempotencyKey: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			longID = result.Message.ID
		}
		if i == 2 {
			privateID = result.Message.ID
		}
		conversation, err = service.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	page, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{Limit: 2})
	if err != nil || len(page.Messages) != 2 || page.Messages[1].ID != longID || page.NextBeforeSequence != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	older, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{BeforeSequence: page.NextBeforeSequence, Limit: 2})
	if err != nil || len(older.Messages) != 1 || older.Messages[0].Sequence != 1 {
		t.Fatalf("older=%+v err=%v", older, err)
	}
	var reconstructed strings.Builder
	for offset := 0; ; {
		page, err = ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{MessageID: longID, OffsetBytes: offset})
		if err != nil {
			t.Fatal(err)
		}
		excerpt := page.Messages[0]
		if !utf8.ValidString(excerpt.Content) || len(excerpt.Content) > 4096 {
			t.Fatal("invalid bounded UTF-8 excerpt")
		}
		reconstructed.WriteString(excerpt.Content)
		if excerpt.NextOffsetBytes == nil {
			break
		}
		offset = *excerpt.NextOffsetBytes
	}
	if reconstructed.String() != original {
		t.Fatal("could not recover exact original")
	}
	for _, request := range []ConversationHistoryReadRequest{{MessageID: privateID}, {MessageID: longID, OffsetBytes: 4096}, {OffsetBytes: 1}, {Limit: 11}} {
		if _, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, request); err == nil {
			t.Fatalf("invalid/private read accepted: %+v", request)
		}
	}
	owner.ID = "other"
	if _, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{}); err == nil {
		t.Fatal("foreign owner accepted")
	}
	scope.ID = "other"
	if _, err := ReadConversationHistory(t.Context(), service, scope, conversation.Owner, conversation.ID, viewer, ConversationHistoryReadRequest{}); err == nil {
		t.Fatal("foreign tenant accepted")
	}
}

// TestConversationHistoryStores checks the conversation-scoped approval and
// Run listings on every store, with approvals proposed by loop steps.
func TestConversationHistoryStores(t *testing.T) {
	for _, name := range []string{"sqlite", "postgres"} {
		t.Run(name, func(t *testing.T) {
			var store interface {
				KernelStore
				ActionStore
			}
			switch name {
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
		created, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: "Review", Source: RunSourceManual, ParentRunID: parent, Context: context})
		if err != nil {
			t.Fatal(err)
		}
		// A loop proposal is fenced by the Run lease, so each Run is
		// claimed before the next one is queued.
		run, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "history-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute})
		if err != nil || run == nil || run.ID != created.ID {
			t.Fatalf("claim %s = %#v, %v", created.ID, run, err)
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
		proposal.Call.Origin, proposal.Call.Execution, proposal.Call.TurnID = ActionOriginLoop, ActionExecutionLoop, "step-1"
		proposal.Call.InvocationDigest = ComputeActionInvocationDigest(proposal.Call)
		proposal.Lease = &AgentRunLeaseGuard{WorkerID: "history-worker", Now: time.Now().UTC()}
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
