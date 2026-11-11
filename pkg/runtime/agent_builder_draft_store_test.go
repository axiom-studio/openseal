package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// agentBuilderDraftSeed is one scope's builder conversations: ana's "made"
// (its approved create card succeeded), "one" and "two" (in another space,
// newest), and bo's "theirs".
type agentBuilderDraftSeed struct {
	scope                     Scope
	made, one, two, theirs    string
	defaultSpace, secondSpace string
}

func seedAgentBuilderDrafts(t *testing.T, store KernelStore) agentBuilderDraftSeed {
	t.Helper()
	f, card := cardApproval(t, store, "create", createAgentCard(), ApprovalTimeoutExpire)
	ctx := f.ctx
	service := NewConversationService(store.(ConversationStore))
	second, err := NewSpaceService(store.(SpaceStore)).CreateSpace(ctx, CreateSpaceRequest{Scope: f.scope, Name: "Second", Look: DefaultSpaceLook, CreatedBy: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	start := func(requester, key, spaceID string) *Conversation {
		t.Helper()
		result, err := StartAgentBuilderConversation(ctx, service, StartAgentBuilderRequest{
			Scope: f.scope, Requester: ConversationParticipant{Type: ConversationParticipantUser, ID: requester},
			Prompt: "Build " + key, Parameters: AgentBuilderParameters{Name: key}, SpaceID: spaceID, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Distinct updated_at values keep the expected order independent of clock resolution.
		time.Sleep(2 * time.Millisecond)
		return result.Conversation
	}
	made := start("ana", "made", "")
	one := start("ana", "one", "")
	theirs := start("bo", "theirs", "")
	two := start("ana", "two", second.ID)
	// A plain chat with the builder is nobody's draft.
	plain, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: f.scope, Owner: made.Owner, Title: "Chat", IdempotencyKey: "plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: f.scope, ConversationID: plain.ID, ExpectedRevision: plain.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "ana"},
		Intent: MessageIntentUpdate, Content: "Hi", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "plain-1",
	}); err != nil {
		t.Fatal(err)
	}

	// The card's Run belongs to "made"; approving it and succeeding its call creates the Agent.
	run := f.currentRun()
	withConversation := cloneAgentRun(run)
	withConversation.Context = map[string]interface{}{"conversationId": made.ID}
	withConversation.Revision++
	if _, err := store.UpdateAgentRunWithEvent(ctx, withConversation, run.Revision, f.event("run-conversation", "run.updated"), nil); err != nil {
		t.Fatal(err)
	}
	if created, err := createdAgentBuilderConversation(ctx, store, &ActionCall{Scope: f.scope, ApprovalID: card.ID}, withConversation); err != nil || created != "" {
		t.Fatalf("a pending card finished its draft: %q, %v", created, err)
	}
	if _, err := f.resolveWith(builderApprovalHost{calls: new(int)}, card, ApprovalDecisionApprove, loopApprover, ""); err != nil {
		t.Fatal(err)
	}
	at := f.now.Add(2 * time.Second)
	claimed, err := store.ClaimNextAction(ctx, ActionClaim{Scope: f.scope, WorkerID: "worker", Now: at, LeaseDuration: time.Minute, OwnActionID: card.ActionCallID})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	run = f.currentRun()
	created, err := createdAgentBuilderConversation(ctx, store, claimed, run)
	if err != nil || created != made.ID {
		t.Fatalf("created from = %q, %v; want %s", created, err, made.ID)
	}
	succeeded := cloneActionCall(claimed)
	succeeded.Status, succeeded.CompletedAt = ActionCallStatusSucceeded, &at
	succeeded.LeaseOwner, succeeded.LeaseExpiresAt = "", nil
	succeeded.UpdatedAt = at
	succeeded.Revision++
	if _, err := store.PersistActionExecution(ctx, ActionExecutionRecord{
		Call: succeeded, ExpectedCallRevision: claimed.Revision, ExpectedRunRevision: run.Revision, WorkerID: "worker", Now: at,
		Event: f.event("created", "action.succeeded"), CreatedFromBuilderConversationID: created,
	}); err != nil {
		t.Fatal(err)
	}
	return agentBuilderDraftSeed{scope: f.scope, made: made.ID, one: one.ID, two: two.ID, theirs: theirs.ID, defaultSpace: one.SpaceID, secondSpace: second.ID}
}

// countingDraftReader records which conversations a drafts list read.
type countingDraftReader struct {
	*ConversationService
	store         KernelStore
	conversations int
	statusReads   []string
}

func (r *countingDraftReader) ListConversations(ctx context.Context, filter ConversationFilter) ([]*Conversation, error) {
	r.conversations++
	return r.ConversationService.ListConversations(ctx, filter)
}

func (r *countingDraftReader) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	return r.store.ListAgentRuns(ctx, filter)
}

func (r *countingDraftReader) ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error) {
	r.statusReads = append(r.statusReads, filter.ConversationID)
	return r.store.ListApprovals(ctx, filter)
}

func assertAgentBuilderDrafts(t *testing.T, store KernelStore, seed agentBuilderDraftSeed) {
	t.Helper()
	list := func(requester string, filter AgentBuilderDraftFilter, want ...string) {
		t.Helper()
		reader := &countingDraftReader{ConversationService: NewConversationService(store.(ConversationStore)), store: store}
		filter.Scope = seed.scope
		filter.Requester = ConversationParticipant{Type: ConversationParticipantUser, ID: requester}
		drafts, err := ListAgentBuilderDrafts(context.Background(), reader, filter)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(drafts))
		for _, draft := range drafts {
			got = append(got, draft.ConversationID)
		}
		// One conversation query; only the listed drafts are read for status.
		if fmt.Sprint(got) != fmt.Sprint(want) || reader.conversations != 1 || fmt.Sprint(reader.statusReads) != fmt.Sprint(got) {
			t.Fatalf("%s %+v = %v (conversation queries %d, status reads %v), want %v", requester, filter, got, reader.conversations, reader.statusReads, want)
		}
	}
	list("ana", AgentBuilderDraftFilter{}, seed.two, seed.one)
	list("ana", AgentBuilderDraftFilter{Limit: 1}, seed.two)
	list("ana", AgentBuilderDraftFilter{Limit: 1, Offset: 1}, seed.one)
	list("ana", AgentBuilderDraftFilter{Offset: 2})
	list("ana", AgentBuilderDraftFilter{SpaceID: seed.secondSpace}, seed.two)
	list("ana", AgentBuilderDraftFilter{SpaceID: seed.defaultSpace}, seed.one)
	list("bo", AgentBuilderDraftFilter{}, seed.theirs)
	list("cy", AgentBuilderDraftFilter{})
}

func TestAgentBuilderDraftsAreSelectedByTheStore(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		assertAgentBuilderDrafts(t, store, seedAgentBuilderDrafts(t, store))
	})
}

// A store written before builder_draft_requester existed lists the same
// drafts once the migration has filled the column.
func TestAgentBuilderDraftMigrationFillsExistingConversations(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "drafts.db")
		store, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		seed := seedAgentBuilderDrafts(t, store)
		if _, err := store.db.Exec(`DROP INDEX idx_conversations_builder_draft; ALTER TABLE conversations DROP COLUMN builder_draft_requester`); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		assertAgentBuilderDrafts(t, reopened, seed)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
		}
		ctx := context.Background()
		schema := "openseal_builder_drafts_" + uuid.NewString()[:8]
		store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
			_ = store.Close()
		})
		seed := seedAgentBuilderDrafts(t, store)
		if err := store.RollbackPostgresMigrations(ctx, agentBuilderDraftMigrationVersion-1); err != nil {
			t.Fatal(err)
		}
		var columns int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'conversations' AND column_name = 'builder_draft_requester'`, schema).Scan(&columns); err != nil || columns != 0 {
			t.Fatalf("rollback kept the column: %d, %v", columns, err)
		}
		reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		assertAgentBuilderDrafts(t, reopened, seed)
	})
}
