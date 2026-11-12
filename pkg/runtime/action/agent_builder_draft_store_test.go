package action_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// agentBuilderDraftSeed is one scope's builder conversations: ana's "made"
// (its approved create card succeeded), "one" and "two" (in another space,
// newest), and bo's "theirs".
type agentBuilderDraftSeed struct {
	scope                     models.Scope
	made, one, two, theirs    string
	defaultSpace, secondSpace string
}

func seedAgentBuilderDrafts(t *testing.T, store models.KernelStore) agentBuilderDraftSeed {
	t.Helper()
	f, card := cardApproval(t, store, "create", createAgentCard(), models.ApprovalTimeoutExpire)
	ctx := f.Ctx
	service := runtime.NewConversationService(store.(models.ConversationStore))
	// Each requester's first space is their default; the plain chat has no
	// person behind it and goes to the empty owner's.
	seedTestSpaceFor(t, store.(models.SpaceStore), f.Scope, "ana")
	seedTestSpaceFor(t, store.(models.SpaceStore), f.Scope, "bo")
	seedTestSpace(t, store.(models.SpaceStore), f.Scope)
	second, err := portfolio.NewSpaceService(store.(models.SpaceStore)).CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: f.Scope, Name: "Second", Look: models.DefaultSpaceLook, OwnerID: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	start := func(requester, key, spaceID string) *models.Conversation {
		t.Helper()
		result, err := runtime.StartAgentBuilderConversation(ctx, service, models.StartAgentBuilderRequest{
			Scope: f.Scope, Requester: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: requester},
			Prompt: "Build " + key, Parameters: models.AgentBuilderParameters{Name: key}, SpaceID: spaceID, IdempotencyKey: key,
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
	plain, _, err := service.CreateConversation(ctx, runtime.CreateConversationRequest{
		Scope: f.Scope, Owner: made.Owner, Title: "Chat", IdempotencyKey: "plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PostChannelMessage(ctx, runtime.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: plain.ID, ExpectedRevision: plain.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "ana"},
		Intent: models.MessageIntentUpdate, Content: "Hi", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "plain-1",
	}); err != nil {
		t.Fatal(err)
	}

	// The card's Run belongs to "made"; approving it and succeeding its call creates the Agent.
	run := f.CurrentRun()
	withConversation := models.CloneAgentRun(run)
	withConversation.Context = map[string]interface{}{"conversationId": made.ID}
	withConversation.Revision++
	if _, err := store.UpdateAgentRunWithEvent(ctx, withConversation, run.Revision, f.Event("run-conversation", "run.updated"), nil); err != nil {
		t.Fatal(err)
	}
	if created, err := action.CreatedAgentBuilderConversation(ctx, store, &models.ActionCall{Scope: f.Scope, ApprovalID: card.ID}, withConversation); err != nil || created != "" {
		t.Fatalf("a pending card finished its draft: %q, %v", created, err)
	}
	if _, err := f.ResolveWith(builderApprovalHost{calls: new(int)}, card, models.ApprovalDecisionApprove, actiontest.LoopApprover, ""); err != nil {
		t.Fatal(err)
	}
	at := f.Now.Add(2 * time.Second)
	claimed, err := store.ClaimNextAction(ctx, models.ActionClaim{Scope: f.Scope, WorkerID: "worker", Now: at, LeaseDuration: time.Minute, OwnActionID: card.ActionCallID})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	run = f.CurrentRun()
	created, err := action.CreatedAgentBuilderConversation(ctx, store, claimed, run)
	if err != nil || created != made.ID {
		t.Fatalf("created from = %q, %v; want %s", created, err, made.ID)
	}
	succeeded := models.CloneActionCall(claimed)
	succeeded.Status, succeeded.CompletedAt = models.ActionCallStatusSucceeded, &at
	succeeded.LeaseOwner, succeeded.LeaseExpiresAt = "", nil
	succeeded.UpdatedAt = at
	succeeded.Revision++
	if _, err := store.PersistActionExecution(ctx, models.ActionExecutionRecord{
		Call: succeeded, ExpectedCallRevision: claimed.Revision, ExpectedRunRevision: run.Revision, WorkerID: "worker", Now: at,
		Event: f.Event("created", "action.succeeded"), CreatedFromBuilderConversationID: created,
	}); err != nil {
		t.Fatal(err)
	}
	return agentBuilderDraftSeed{scope: f.Scope, made: made.ID, one: one.ID, two: two.ID, theirs: theirs.ID, defaultSpace: one.SpaceID, secondSpace: second.ID}
}

// countingDraftReader records which conversations a drafts list read.
type countingDraftReader struct {
	*runtime.ConversationService
	store         models.KernelStore
	conversations int
	statusReads   []string
}

func (r *countingDraftReader) ListConversations(ctx context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	r.conversations++
	return r.ConversationService.ListConversations(ctx, filter)
}

func (r *countingDraftReader) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	return r.store.ListAgentRuns(ctx, filter)
}

func (r *countingDraftReader) ListApprovals(ctx context.Context, filter models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error) {
	r.statusReads = append(r.statusReads, filter.ConversationID)
	return r.store.ListApprovals(ctx, filter)
}

func assertAgentBuilderDrafts(t *testing.T, store models.KernelStore, seed agentBuilderDraftSeed) {
	t.Helper()
	list := func(requester string, filter runtime.AgentBuilderDraftFilter, want ...string) {
		t.Helper()
		reader := &countingDraftReader{ConversationService: runtime.NewConversationService(store.(models.ConversationStore)), store: store}
		filter.Scope = seed.scope
		filter.Requester = models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: requester}
		drafts, err := runtime.ListAgentBuilderDrafts(context.Background(), reader, filter)
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
	list("ana", runtime.AgentBuilderDraftFilter{}, seed.two, seed.one)
	list("ana", runtime.AgentBuilderDraftFilter{Limit: 1}, seed.two)
	list("ana", runtime.AgentBuilderDraftFilter{Limit: 1, Offset: 1}, seed.one)
	list("ana", runtime.AgentBuilderDraftFilter{Offset: 2})
	list("ana", runtime.AgentBuilderDraftFilter{SpaceID: seed.secondSpace}, seed.two)
	list("ana", runtime.AgentBuilderDraftFilter{SpaceID: seed.defaultSpace}, seed.one)
	list("bo", runtime.AgentBuilderDraftFilter{}, seed.theirs)
	list("cy", runtime.AgentBuilderDraftFilter{})
}

func TestAgentBuilderDraftsAreSelectedByTheStore(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		assertAgentBuilderDrafts(t, store, seedAgentBuilderDrafts(t, store))
	})
}

// A store written before builder_draft_requester existed lists the same
// drafts once the migration has filled the column.
func TestAgentBuilderDraftMigrationFillsExistingConversations(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "drafts.db")
		store, err := sqlite.NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		seed := seedAgentBuilderDrafts(t, store)
		if _, err := openTestSQLiteDB(t, path).Exec(`DROP INDEX idx_conversations_builder_draft; ALTER TABLE conversations DROP COLUMN builder_draft_requester`); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := sqlite.NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		assertAgentBuilderDrafts(t, reopened, seed)
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "openseal_builder_drafts_")
		dsn := pgSchema.DSN
		schema := pgSchema.Name
		ctx := context.Background()
		store, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
			_ = store.Close()
		})
		seed := seedAgentBuilderDrafts(t, store)
		// The migration ledger names the migration that added the column.
		var draftMigration int64
		if err := pgSchema.DB.QueryRowContext(ctx, `SELECT version FROM `+pgSchema.Table("schema_migrations")+` WHERE name = 'agent builder draft requester'`).Scan(&draftMigration); err != nil {
			t.Fatal(err)
		}
		if err := store.RollbackPostgresMigrations(ctx, draftMigration-1); err != nil {
			t.Fatal(err)
		}
		var columns int
		if err := pgSchema.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'conversations' AND column_name = 'builder_draft_requester'`, schema).Scan(&columns); err != nil || columns != 0 {
			t.Fatalf("rollback kept the column: %d, %v", columns, err)
		}
		reopened, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		assertAgentBuilderDrafts(t, reopened, seed)
	})
}

// seedTestSpaceFor, seedTestSpace and openTestSQLiteDB are copies of
// runtime's test helpers, which the action tests cannot see. Change the
// copies together.

// seedTestSpaceFor creates a space owned by ownerID. The owner's first space
// in the scope is their default space. An owner who already has a default
// space keeps it and gets no second one, so a fixture that is built more than
// once on a store can seed every time.
func seedTestSpaceFor(t testing.TB, store models.SpaceStore, scope models.Scope, ownerID string) *models.Space {
	t.Helper()
	if existing, err := store.DefaultSpace(context.Background(), scope, ownerID); err == nil {
		return existing
	} else if !errors.Is(err, models.ErrNoDefaultSpace) {
		t.Fatalf("seed space: %v", err)
	}
	space, err := portfolio.NewSpaceService(store).CreateSpace(context.Background(), portfolio.CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: ownerID,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return space
}

// seedTestSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go to.
func seedTestSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	return seedTestSpaceFor(t, store, scope, "")
}

// openTestSQLiteDB opens the test's own connection to the SQLite file at
// path, closed when the test ends.
func openTestSQLiteDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
