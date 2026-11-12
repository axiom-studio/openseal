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
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// The helpers in this file are for the in-package conversation tests,
// which cannot import conversationtest (it imports conversation). They
// copy runtime's and conversationtest's SQLite openers, space seed and
// thread start post, and the final-answer action calls and run context
// section reader of runtime's conversation run tests. Change the copies
// together.

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

// newTestSQLiteStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends. Use it instead of
// kerneltest.NewStore when the test reads or writes rows no store method
// offers.
func newTestSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, openTestSQLiteDB(t, path)
}

// seedTestSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go to.
func seedTestSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	return seedTestSpaceFor(t, store, scope, "")
}

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

func threadStartPost(t *testing.T, service *ConversationService, conversation *models.Conversation, req PostChannelMessageRequest) *models.ChannelMessage {
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

// fakeActionCalls is the governed call store the handler reads; tool
// entries reference its calls by id.
type fakeActionCalls struct {
	mu    sync.Mutex
	calls map[string]*models.ActionCall
}

func (a *fakeActionCalls) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call, ok := a.calls[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	copied := *call
	return &copied, nil
}

func runContextSectionNames(t *testing.T, note *models.SessionEntry) map[string]bool {
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
