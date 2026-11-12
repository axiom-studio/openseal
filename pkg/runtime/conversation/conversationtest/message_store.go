package conversationtest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// CountedPointStore counts the channel messages read one by one and fails
// the read of message FailID with Err.
type CountedPointStore struct {
	models.ConversationStore
	Points int
	FailID string
	Err    error
}

func (s *CountedPointStore) GetChannelMessage(ctx context.Context, scope models.Scope, conversationID, id string) (*models.ChannelMessage, error) {
	s.Points++
	if id == s.FailID && s.Err != nil {
		return nil, s.Err
	}
	return s.ConversationStore.GetChannelMessage(ctx, scope, conversationID, id)
}

// CountedBatchStore counts the batch reads of channel messages, records
// their IDs unless CountOnly is set, and fails a batch holding FailID
// with Err.
type CountedBatchStore struct {
	*CountedPointStore
	Batch     models.ChannelMessageBatchStore
	Batches   [][]string
	Calls     int
	CountOnly bool
	FailID    string
	Err       error
}

func (s *CountedBatchStore) GetChannelMessagesByIDs(ctx context.Context, scope models.Scope, conversationID string, ids []string) ([]*models.ChannelMessage, error) {
	s.Calls++
	if !s.CountOnly {
		s.Batches = append(s.Batches, append([]string(nil), ids...))
	}
	for _, id := range ids {
		if id == s.FailID && s.Err != nil {
			return nil, s.Err
		}
	}
	return s.Batch.GetChannelMessagesByIDs(ctx, scope, conversationID, ids)
}

// NewCountedBatchStore counts the point and batch message reads of store,
// which must also be a ChannelMessageBatchStore.
func NewCountedBatchStore(store models.ConversationStore) *CountedBatchStore {
	return &CountedBatchStore{CountedPointStore: &CountedPointStore{ConversationStore: store}, Batch: store.(models.ChannelMessageBatchStore)}
}

// MessageBatchStores runs test on a SQLite file store.
func MessageBatchStores(t *testing.T, test func(*testing.T, models.KernelStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.New(filepath.Join(t.TempDir(), "message-batch.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		test(t, store)
	})
}

// MessageBatchConversation creates team conversation id in scope.
func MessageBatchConversation(t testing.TB, store models.ConversationStore, scope models.Scope, id string) *models.Conversation {
	t.Helper()
	SeedSpace(t, store.(models.SpaceStore), scope)
	created, _, err := conversation.NewService(store).CreateConversation(context.Background(), conversation.CreateConversationRequest{
		ID: id, Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, Title: "Batch visibility", IdempotencyKey: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// CommitChannelMessage commits message as the next update of *target
// straight to the store and moves *target to the committed revision.
func CommitChannelMessage(t testing.TB, store models.ConversationStore, target **models.Conversation, message *models.ChannelMessage) {
	t.Helper()
	current := *target
	message.Scope, message.ConversationID = current.Scope, current.ID
	message.Sequence, message.CreatedAt, message.IdempotencyKey = current.LastSequence+1, time.Now().UTC(), message.ID
	message.Sender = models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "sender"}
	message.Intent, message.Content = models.MessageIntentUpdate, "Visibility fixture "+message.ID
	if message.Audience.Kind == "" {
		message.Audience.Kind = models.ConversationAudienceChannel
	}
	next := models.CloneConversation(current)
	next.Revision, next.LastSequence, next.UpdatedAt = current.Revision+1, message.Sequence, message.CreatedAt
	committed, err := store.CommitChannelMessage(context.Background(), models.ChannelMessageCommitRecord{Conversation: next, ExpectedRevision: current.Revision, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	*target = committed.Conversation
}
