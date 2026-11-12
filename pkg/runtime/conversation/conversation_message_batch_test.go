package conversation_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func channelMessageBatchHistory(t testing.TB, store models.KernelStore, count int) (*models.Conversation, []*models.AgentRun) {
	t.Helper()
	conversationStore := store.(models.ConversationStore)
	conversation := conversationtest.MessageBatchConversation(t, conversationStore, models.Scope{Kind: "tenant", ID: "batch-history"}, "history")
	conversationtest.CommitChannelMessage(t, conversationStore, &conversation, &models.ChannelMessage{ID: "parent", Audience: models.ConversationAudience{Kind: models.ConversationAudienceRoles, Roles: []string{"reader"}}})
	runs := make([]*models.AgentRun, 0, count)
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("message-%03d", index)
		conversationtest.CommitChannelMessage(t, conversationStore, &conversation, &models.ChannelMessage{ID: id, ThreadRootID: "parent", ReplyToMessageID: "parent"})
		run, err := portfolio.NewPortfolioService(store).CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
			Scope: conversation.Scope, Owner: conversation.Owner, Kind: models.RunKindConversation, ConcurrencyKey: conversation.ID,
			Goal: "Historical reply", Source: models.RunSourceChat, Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: id},
		})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	return conversation, runs
}

func TestConversationMessageBatchReducesIdleHistoricalPollReads(t *testing.T) {
	store := conversationtest.NewStore(t)
	conversation, runs := channelMessageBatchHistory(t, store, 100)
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	counted := conversationtest.NewCountedBatchStore(store)
	changes, err := runtimeconversation.NewConversationChangeService(counted, store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := changes.ListChanges(t.Context(), runtimeconversation.ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Limit: 500, Viewer: &viewer})
	if err != nil || len(initial.Runs) != 100 {
		t.Fatalf("initial historical projection = %#v, %v", initial, err)
	}
	counted.Points, counted.Batches = 0, nil
	idle, err := changes.ListChanges(t.Context(), runtimeconversation.ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Cursor: initial.Cursor, Viewer: &viewer})
	if err != nil || idle.HasChanges || counted.Points != 0 || len(counted.Batches) != 2 || len(counted.Batches[0]) != 100 || !reflect.DeepEqual(counted.Batches[1], []string{"parent"}) {
		t.Fatalf("idle poll retained historical point reads: changes=%#v err=%v points=%d batches=%v", idle, err, counted.Points, counted.Batches)
	}
	legacy := &conversationtest.CountedPointStore{ConversationStore: store}
	legacyChanges, _ := runtimeconversation.NewConversationChangeService(legacy, store)
	baseline, err := legacyChanges.FilterVisibleRuns(t.Context(), conversation.Scope, conversation.ID, runs, viewer)
	if err != nil || len(baseline) != 100 || legacy.Points != 200 {
		t.Fatalf("custom store compatibility changed: visible=%d err=%v reads=%d", len(baseline), err, legacy.Points)
	}
	viewer.Roles = nil
	counted.Points, counted.Batches = 0, nil
	hidden, err := changes.ListChanges(t.Context(), runtimeconversation.ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Cursor: idle.Cursor, Viewer: &viewer})
	if err != nil || !hidden.RunsChanged || len(hidden.Runs) != 0 || counted.Points != 0 || len(counted.Batches) != 2 {
		t.Fatalf("poll reused old viewer authorization: changes=%#v err=%v batches=%v", hidden, err, counted.Batches)
	}
	viewer.Roles = []string{"reader"}
	restored, err := changes.ListChanges(t.Context(), runtimeconversation.ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Cursor: hidden.Cursor, Viewer: &viewer})
	if err != nil || !restored.RunsChanged || len(restored.Runs) != 100 {
		t.Fatalf("poll did not re-evaluate current viewer authorization: %#v, %v", restored, err)
	}
}

func TestConversationMessageBatchPreservesLinkedParentVisibilityAndScope(t *testing.T) {
	conversationtest.MessageBatchStores(t, func(t *testing.T, store models.KernelStore) {
		conversationStore := store.(models.ConversationStore)
		scope := models.Scope{Kind: "tenant", ID: "visibility"}
		conversation := conversationtest.MessageBatchConversation(t, conversationStore, scope, "channel")
		private := models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantUser, ID: "other"}}}
		for _, message := range []*models.ChannelMessage{
			{ID: "public"}, {ID: "private", Audience: private}, {ID: "parent-with-grandparent", ThreadRootID: "private"},
			{ID: "visible", ThreadRootID: "public", ReplyToMessageID: "public"},
			{ID: "direct-private", Audience: private, ThreadRootID: "must-not-read"},
			{ID: "hidden-thread", ThreadRootID: "private", ReplyToMessageID: "must-not-read"},
			{ID: "hidden-reply", ThreadRootID: "public", ReplyToMessageID: "private"},
			{ID: "missing-parent", ThreadRootID: "missing"}, {ID: "foreign-conversation", ThreadRootID: "foreign-conversation-parent"},
			{ID: "foreign-tenant", ThreadRootID: "foreign-tenant-parent"}, {ID: "self", ThreadRootID: "self", ReplyToMessageID: "self"},
			{ID: "one-level", ThreadRootID: "parent-with-grandparent"},
		} {
			conversationtest.CommitChannelMessage(t, conversationStore, &conversation, message)
		}
		foreignConversation := conversationtest.MessageBatchConversation(t, conversationStore, scope, "other-channel")
		conversationtest.CommitChannelMessage(t, conversationStore, &foreignConversation, &models.ChannelMessage{ID: "foreign-conversation-parent"})
		foreignTenant := conversationtest.MessageBatchConversation(t, conversationStore, models.Scope{Kind: "tenant", ID: "other-tenant"}, conversation.ID)
		conversationtest.CommitChannelMessage(t, conversationStore, &foreignTenant, &models.ChannelMessage{ID: "foreign-tenant-parent"})
		runs := make([]*models.AgentRun, 0)
		for _, id := range []string{"visible", "direct-private", "hidden-thread", "hidden-reply", "missing-trigger", "missing-parent", "foreign-conversation", "foreign-tenant", "self", "one-level"} {
			runs = append(runs, &models.AgentRun{ID: id, Kind: models.RunKindConversation, RootRunID: id, Context: map[string]interface{}{models.ConversationRunTriggerIDContextKey: id}}, &models.AgentRun{ID: id + "-child", Kind: models.RunKindAgentWork, RootRunID: id})
		}
		viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "viewer"}}
		counted := conversationtest.NewCountedBatchStore(conversationStore)
		counted.FailID, counted.Err = "must-not-read", errors.New("hidden parent must not be queried")
		changes, _ := runtimeconversation.NewConversationChangeService(counted, store)
		visible, err := changes.FilterVisibleRuns(t.Context(), scope, conversation.ID, runs, viewer)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(visible))
		for _, run := range visible {
			ids = append(ids, run.ID)
		}
		if expected := []string{"visible", "visible-child", "self", "self-child", "one-level", "one-level-child"}; !reflect.DeepEqual(ids, expected) || counted.Points != 0 {
			t.Fatalf("batch parent visibility changed: got=%v points=%d", ids, counted.Points)
		}
		legacy := &conversationtest.CountedPointStore{ConversationStore: conversationStore, FailID: "must-not-read", Err: counted.Err}
		legacyChanges, _ := runtimeconversation.NewConversationChangeService(legacy, store)
		baseline, err := legacyChanges.FilterVisibleRuns(t.Context(), scope, conversation.ID, runs, viewer)
		if err != nil || !reflect.DeepEqual(visible, baseline) {
			t.Fatalf("batch differs from custom point-read semantics: %#v, %v", baseline, err)
		}
		counted.FailID = "public"
		if _, err := changes.FilterVisibleRuns(t.Context(), scope, conversation.ID, runs, viewer); !errors.Is(err, counted.Err) {
			t.Fatalf("visible parent read error was swallowed: %v", err)
		}
	})
}

type countedChannelMessageSQLReader struct {
	db      *sql.DB
	queries int
}

func (r *countedChannelMessageSQLReader) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	r.queries++
	return r.db.QueryContext(ctx, query, args...)
}

func TestConversationMessageBatchSQLiteQueriesExactIDsOnceAndClones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exact-batch.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := conversationtest.OpenSQLiteDB(t, path)
	defer store.Close()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	conversation := conversationtest.MessageBatchConversation(t, store, scope, "channel")
	conversationtest.CommitChannelMessage(t, store, &conversation, &models.ChannelMessage{ID: "shared"})
	otherConversation := conversationtest.MessageBatchConversation(t, store, scope, "other")
	conversationtest.CommitChannelMessage(t, store, &otherConversation, &models.ChannelMessage{ID: "shared"})
	otherTenant := conversationtest.MessageBatchConversation(t, store, models.Scope{Kind: "tenant", ID: "two"}, "channel")
	conversationtest.CommitChannelMessage(t, store, &otherTenant, &models.ChannelMessage{ID: "shared"})
	reader := &countedChannelMessageSQLReader{db: sqlDB}
	selected, err := sqlstore.GetChannelMessageBatch(t.Context(), reader, "channel_messages", scope, conversation.ID, []string{"shared", "missing", "shared"}, false)
	if err != nil || reader.queries != 1 || len(selected) != 1 || selected[0].Scope != scope || selected[0].ConversationID != conversation.ID {
		t.Fatalf("exact scoped batch query = %#v, %v; reads=%d", selected, err, reader.queries)
	}
	selected[0].Content = "caller changed its decoded message"
	fresh, err := store.GetChannelMessagesByIDs(t.Context(), scope, conversation.ID, []string{"shared"})
	if err != nil || len(fresh) != 1 || fresh[0].Content == selected[0].Content {
		t.Fatalf("caller mutation affected a later batch: %#v, %v", fresh, err)
	}
	if _, err := store.GetChannelMessagesByIDs(t.Context(), scope, conversation.ID, make([]string, models.MaximumChannelMessageBatchSize+1)); !errors.Is(err, models.ErrInvalidConversation) {
		t.Fatalf("unbounded SQL batch accepted: %v", err)
	}
	reader.queries = 0
	empty, err := sqlstore.GetChannelMessageBatch(t.Context(), reader, "channel_messages", scope, conversation.ID, nil, false)
	if err != nil || len(empty) != 0 || reader.queries != 0 {
		t.Fatalf("empty batch performed a database read: %#v %v %d", empty, err, reader.queries)
	}
}

func TestConversationMessageBatchChunksAndDeduplicatesExactIDs(t *testing.T) {
	store := conversationtest.NewStore(t)
	conversation, runs := channelMessageBatchHistory(t, store, 401)
	runs = append(runs, runs[0])
	counted := conversationtest.NewCountedBatchStore(store)
	changes, _ := runtimeconversation.NewConversationChangeService(counted, store)
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	visible, err := changes.FilterVisibleRuns(t.Context(), conversation.Scope, conversation.ID, runs, viewer)
	if err != nil || len(visible) != len(runs) || len(counted.Batches) != 4 {
		t.Fatalf("chunked visibility = %d, %v; batch sizes=%v", len(visible), err, counted.Batches)
	}
	for index, size := range []int{200, 200, 1, 1} {
		if len(counted.Batches[index]) != size {
			t.Fatalf("chunk%d has%d IDs, expected%d", index, len(counted.Batches[index]), size)
		}
	}
}

func BenchmarkConversationMessageVisibilityHistory(b *testing.B) {
	store, err := sqlite.NewSQLiteStore(filepath.Join(b.TempDir(), "poll-batch.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	conversation, runs := channelMessageBatchHistory(b, store, 100)
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	for _, batched := range []bool{false, true} {
		name := "point"
		if batched {
			name = "batch"
		}
		b.Run(name, func(b *testing.B) {
			points := &conversationtest.CountedPointStore{ConversationStore: store}
			var conversationStore models.ConversationStore = points
			var batch *conversationtest.CountedBatchStore
			if batched {
				batch = &conversationtest.CountedBatchStore{CountedPointStore: points, Batch: store, CountOnly: true}
				conversationStore = batch
			}
			changes, _ := runtimeconversation.NewConversationChangeService(conversationStore, store)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if visible, err := changes.FilterVisibleRuns(context.Background(), conversation.Scope, conversation.ID, runs, viewer); err != nil || len(visible) != 100 {
					b.Fatalf("historical visibility = %d, %v", len(visible), err)
				}
			}
			reads := points.Points
			if batch != nil {
				reads += batch.Calls
			}
			b.ReportMetric(float64(reads)/float64(b.N), "message_reads/op")
		})
	}
}
