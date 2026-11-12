package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// The helpers in this file copy runtime's store openers, space seed,
// SQL store runner, thread start fixtures, final-answer action calls and
// run context section reader for the conversation tests, which are about
// to move with the conversation code and cannot see runtime's test
// helpers. Change the copies together.

// newConversationTestStore returns an empty in-memory kernel store that is
// closed when the test ends.
func newConversationTestStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// openConversationTestDB opens the test's own connection to the SQLite file at
// path, closed when the test ends.
func openConversationTestDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newConversationTestSQLiteStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends. Use it instead of
// newConversationTestStore when the test reads or writes rows no store method
// offers.
func newConversationTestSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, openConversationTestDB(t, path)
}

// seedConversationTestSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go to.
func seedConversationTestSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	return seedConversationTestSpaceFor(t, store, scope, "")
}

// seedConversationTestSpaceFor creates a space owned by ownerID. The owner's first space
// in the scope is their default space. An owner who already has a default
// space keeps it and gets no second one, so a fixture that is built more than
// once on a store can seed every time.
func seedConversationTestSpaceFor(t testing.TB, store models.SpaceStore, scope models.Scope, ownerID string) *models.Space {
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

type sqlTestStore = storetest.ConversationTaskStore

type sqlStoreFixture struct {
	store  sqlTestStore
	reopen func(*testing.T) sqlTestStore
}

func forSQLStores(t *testing.T, test func(*testing.T, sqlStoreFixture)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conversation-tasks.db")
		open := func(t *testing.T) sqlTestStore {
			store, err := sqlite.NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
		test(t, sqlStoreFixture{store: open(t), reopen: open})
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "openseal_task_sql_")
		dsn := pgSchema.DSN
		schema := pgSchema.Name
		open := func(t *testing.T) sqlTestStore {
			store, err := postgres.NewPostgresStore(t.Context(), dsn, postgres.WithPostgresSchema(schema))
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
			_, _ = cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		})
		test(t, sqlStoreFixture{store: store, reopen: open})
	})
}

type ThreadStartStore interface {
	models.KernelStore
	models.ConversationStore
}

func ForThreadStartStores(t *testing.T, test func(*testing.T, ThreadStartStore, func(*testing.T) ThreadStartStore)) {
	t.Helper()
	forSQLStores(t, func(t *testing.T, fixture sqlStoreFixture) {
		test(t, fixture.store.(ThreadStartStore), func(t *testing.T) ThreadStartStore {
			return fixture.reopen(t).(ThreadStartStore)
		})
	})
}

func ThreadStartConversation(t *testing.T, store models.ConversationStore) *models.Conversation {
	t.Helper()
	seedConversationTestSpace(t, store.(models.SpaceStore), models.Scope{Kind: "tenant", ID: "thread-start"})
	conversation, _, err := NewConversationService(store).CreateConversation(t.Context(), CreateConversationRequest{
		Scope: models.Scope{Kind: "tenant", ID: "thread-start"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
		Title: "Thread start contracts", IdempotencyKey: "thread-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func ThreadStartPost(t *testing.T, service *ConversationService, conversation *models.Conversation, req PostChannelMessageRequest) *models.ChannelMessage {
	t.Helper()
	current, err := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Scope, req.ConversationID, req.ExpectedRevision = conversation.Scope, conversation.ID, current.Revision
	if req.ID == "" {
		req.ID = req.IdempotencyKey
	}
	if req.Sender.Type == "" {
		req.Sender = models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user"}
	}
	if req.Intent == "" {
		req.Intent = models.MessageIntentQuestion
	}
	if req.Content == "" {
		req.Content = req.IdempotencyKey
	}
	if req.Audience.Kind == "" {
		req.Audience.Kind = models.ConversationAudienceChannel
	}
	result, err := service.PostChannelMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

func ThreadStartMessageIDs(messages []*models.ChannelMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func ThreadStartHistoryIDs(page *ConversationHistoryReadResult) []string {
	if page == nil {
		return nil
	}
	ids := make([]string, 0, len(page.Messages))
	for _, message := range page.Messages {
		ids = append(ids, message.ID)
	}
	return ids
}

// receiptActionCalls is the governed call store the handler reads; tool
// entries reference its calls by id.
type receiptActionCalls struct {
	mu    sync.Mutex
	calls map[string]*models.ActionCall
}

func (a *receiptActionCalls) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call, ok := a.calls[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	copied := *call
	return &copied, nil
}

func contextNoteSectionNames(t *testing.T, note *models.SessionEntry) map[string]bool {
	t.Helper()
	if note == nil {
		return map[string]bool{}
	}
	var data struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(note.Note.Data, &data); err != nil {
		t.Fatalf("note data: %v", err)
	}
	names := make(map[string]bool, len(data.Sections))
	for name := range data.Sections {
		names[name] = true
	}
	return names
}
