//go:build integration

package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConversationMessageBatchPostgresScopesExactIDsAndBoundsHistoricalReads(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("openseal_message_batch_"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	conversation, runs := channelMessageBatchHistory(t, store, 100)
	foreignConversation := channelMessageBatchConversation(t, store, conversation.Scope, "foreign")
	channelMessageBatchCommit(t, store, &foreignConversation, &ChannelMessage{ID: "message-000"})
	foreignTenant := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "foreign"}, conversation.ID)
	channelMessageBatchCommit(t, store, &foreignTenant, &ChannelMessage{ID: "message-000"})
	selected, err := store.GetChannelMessagesByIDs(ctx, conversation.Scope, conversation.ID, []string{"message-000", "missing", "message-000"})
	if err != nil || len(selected) != 1 || selected[0].Scope != conversation.Scope || selected[0].ConversationID != conversation.ID {
		t.Fatalf("PostgreSQL exact scoped batch = %#v, %v", selected, err)
	}
	counted := newCountedBatchConversationStore(store)
	changes, _ := NewConversationChangeService(counted, store)
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	visible, err := changes.filterVisibleRuns(ctx, conversation.Scope, conversation.ID, runs, viewer)
	if err != nil || len(visible) != 100 || counted.points != 0 || len(counted.batches) != 2 {
		t.Fatalf("PostgreSQL historical visibility retained point reads: visible=%d err=%v points=%d batches=%v", len(visible), err, counted.points, counted.batches)
	}
}
