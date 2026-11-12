package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestConversationHistoryReadsVisibleOriginals(t *testing.T) {
	store := newTestKernelStore(t)
	service := runtimeconversation.NewConversationService(store)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	seedTestSpace(t, store, scope)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: owner.ID}}
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "history", IdempotencyKey: "history"})
	if err != nil {
		t.Fatal(err)
	}
	var longID, privateID string
	original := strings.Repeat("a", 4095) + strings.Repeat("日本語", 1000)
	for i := 0; i < 4; i++ {
		content := fmt.Sprintf("message %d", i)
		audience := models.ConversationAudience{Kind: models.ConversationAudienceChannel}
		if i == 1 {
			content = original
		}
		if i == 2 {
			audience = models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantUser, ID: "private-user"}}}
		}
		result, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "sender"}, Intent: models.MessageIntentUpdate, Content: content, Audience: audience, IdempotencyKey: fmt.Sprint(i)})
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
	page, err := runtimeconversation.ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, runtimeconversation.ConversationHistoryReadRequest{Limit: 2})
	if err != nil || len(page.Messages) != 2 || page.Messages[1].ID != longID || page.NextBeforeSequence != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	older, err := runtimeconversation.ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, runtimeconversation.ConversationHistoryReadRequest{BeforeSequence: page.NextBeforeSequence, Limit: 2})
	if err != nil || len(older.Messages) != 1 || older.Messages[0].Sequence != 1 {
		t.Fatalf("older=%+v err=%v", older, err)
	}
	var reconstructed strings.Builder
	for offset := 0; ; {
		page, err = runtimeconversation.ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, runtimeconversation.ConversationHistoryReadRequest{MessageID: longID, OffsetBytes: offset})
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
	for _, request := range []runtimeconversation.ConversationHistoryReadRequest{{MessageID: privateID}, {MessageID: longID, OffsetBytes: 4096}, {OffsetBytes: 1}, {Limit: 11}} {
		if _, err := runtimeconversation.ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, request); err == nil {
			t.Fatalf("invalid/private read accepted: %+v", request)
		}
	}
	owner.ID = "other"
	if _, err := runtimeconversation.ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, runtimeconversation.ConversationHistoryReadRequest{}); err == nil {
		t.Fatal("foreign owner accepted")
	}
	scope.ID = "other"
	if _, err := runtimeconversation.ReadConversationHistory(t.Context(), service, scope, conversation.Owner, conversation.ID, viewer, runtimeconversation.ConversationHistoryReadRequest{}); err == nil {
		t.Fatal("foreign tenant accepted")
	}
}

// TestConversationHistoryStores checks the conversation-scoped approval and
// Run listings on every store, with approvals proposed by loop steps.
func TestConversationHistoryStores(t *testing.T) {
	for _, name := range []string{"sqlite", "postgres"} {
		t.Run(name, func(t *testing.T) {
			var store interface {
				models.KernelStore
				models.ActionStore
			}
			switch name {
			case "sqlite":
				s, err := sqlite.New(filepath.Join(t.TempDir(), "history.db"))
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
				s, _ := openTestPostgresStore(t, "chat_history_")
				store = s
			}
			testConversationHistory(t, store)
		})
	}
}

func testConversationHistory(t *testing.T, store interface {
	models.KernelStore
	models.ActionStore
}) {
	scope := models.Scope{Kind: "tenant", ID: "one"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}
	portfolio := portfolio.NewPortfolioService(store)
	create := func(scope models.Scope, owner models.ObjectiveOwner, conversation, parent string) *models.AgentRun {
		t.Helper()
		context := map[string]interface{}{}
		if conversation != "" {
			context["conversationId"] = conversation
			context["triggerMessageId"] = "message-" + conversation
		}
		created, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: owner, Goal: "Review", Source: models.RunSourceManual, ParentRunID: parent, Context: context})
		if err != nil {
			t.Fatal(err)
		}
		// A loop proposal is fenced by the Run lease, so each Run is
		// claimed before the next one is queued.
		run, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: scope, WorkerID: "history-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute})
		if err != nil || run == nil || run.ID != created.ID {
			t.Fatalf("claim %s = %#v, %v", created.ID, run, err)
		}
		return run
	}
	root := create(scope, owner, "selected", "")
	child := create(scope, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "delegate"}, "", root.ID)
	unrelated := create(scope, owner, "other", "")
	legacy := create(scope, owner, "", "")
	foreign := create(models.Scope{Kind: "tenant", ID: "two"}, owner, "selected", "")
	for i, run := range []*models.AgentRun{root, child, unrelated, legacy, foreign} {
		proposal := sqliteApprovalProposal(run, fmt.Sprintf("history-call-%d", i), fmt.Sprintf("history-key-%d", i), fmt.Sprintf("history-event-%d", i))
		proposal.Call.Origin, proposal.Call.Execution, proposal.Call.TurnID = models.ActionOriginLoop, models.ActionExecutionLoop, "step-1"
		proposal.Call.InvocationDigest = models.ComputeActionInvocationDigest(proposal.Call)
		proposal.Lease = &models.AgentRunLeaseGuard{WorkerID: "history-worker", Now: time.Now().UTC()}
		if _, err := store.CreateActionProposal(t.Context(), proposal); err != nil {
			t.Fatal(err)
		}
	}
	approvals, err := store.ListApprovals(t.Context(), models.ApprovalFilter{Scope: scope, Owner: &owner, ConversationID: "selected", Limit: 100})
	if err != nil || len(approvals) != 2 {
		t.Fatalf("scoped approvals: %d, %v", len(approvals), err)
	}
	for _, approval := range approvals {
		if approval.ConversationContext == nil || approval.ConversationContext.ConversationID != "selected" || approval.ConversationContext.TriggerMessageID != "message-selected" {
			t.Fatalf("missing inherited context: %#v", approval)
		}
	}
	approvals, err = store.ListApprovals(t.Context(), models.ApprovalFilter{Scope: scope, Owner: &owner, ConversationID: "selected", IncludeUnscoped: true, Limit: 100})
	if err != nil || len(approvals) != 3 {
		t.Fatalf("legacy approvals: %d, %v", len(approvals), err)
	}
	// Projection is response-only; a list must not mutate the durable checkpoint.
	original, err := store.GetApproval(t.Context(), scope, "approval-history-call-0")
	if err != nil || original.ConversationContext != nil {
		t.Fatalf("projection persisted: %#v, %v", original, err)
	}
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, ConversationID: "selected", IncludeDescendants: true, Limit: 100})
	if err != nil || len(runs) != 2 {
		t.Fatalf("scoped run lineage: %d, %v", len(runs), err)
	}
	exact, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, ConversationID: "selected", Limit: 100})
	if err != nil || len(exact) != 1 || exact[0].ID != root.ID {
		t.Fatalf("exact runs: %#v, %v", exact, err)
	}
	page, err := store.ListApprovals(t.Context(), models.ApprovalFilter{Scope: scope, Owner: &owner, ConversationID: "selected", Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("scoped paging: %d, %v", len(page), err)
	}
}
