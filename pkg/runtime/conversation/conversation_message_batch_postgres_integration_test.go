//go:build integration

package conversation_test

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestConversationMessageBatchPostgresScopesExactIDsAndBoundsHistoricalReads(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_message_batch_")
	dsn := pgSchema.DSN
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := postgres.New(ctx, dsn, postgres.WithSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	conversation, runs := channelMessageBatchHistory(t, store, 100)
	foreignConversation := conversationtest.MessageBatchConversation(t, store, conversation.Scope, "foreign")
	conversationtest.CommitChannelMessage(t, store, &foreignConversation, &models.ChannelMessage{ID: "message-000"})
	foreignTenant := conversationtest.MessageBatchConversation(t, store, models.Scope{Kind: "tenant", ID: "foreign"}, conversation.ID)
	conversationtest.CommitChannelMessage(t, store, &foreignTenant, &models.ChannelMessage{ID: "message-000"})
	selected, err := store.GetChannelMessagesByIDs(ctx, conversation.Scope, conversation.ID, []string{"message-000", "missing", "message-000"})
	if err != nil || len(selected) != 1 || selected[0].Scope != conversation.Scope || selected[0].ConversationID != conversation.ID {
		t.Fatalf("PostgreSQL exact scoped batch = %#v, %v", selected, err)
	}
	counted := conversationtest.NewCountedBatchStore(store)
	changes, _ := runtimeconversation.NewConversationChangeService(counted, store)
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	visible, err := changes.FilterVisibleRuns(ctx, conversation.Scope, conversation.ID, runs, viewer)
	if err != nil || len(visible) != 100 || counted.Points != 0 || len(counted.Batches) != 2 {
		t.Fatalf("PostgreSQL historical visibility retained point reads: visible=%d err=%v points=%d batches=%v", len(visible), err, counted.Points, counted.Batches)
	}
}
