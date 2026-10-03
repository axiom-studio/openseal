package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type countedPointConversationStore struct {
	ConversationStore
	points int
	failID string
	err    error
}

func (s *countedPointConversationStore) GetChannelMessage(ctx context.Context, scope Scope, conversationID, id string) (*ChannelMessage, error) {
	s.points++
	if id == s.failID && s.err != nil {
		return nil, s.err
	}
	return s.ConversationStore.GetChannelMessage(ctx, scope, conversationID, id)
}

type countedBatchConversationStore struct {
	*countedPointConversationStore
	batch     ChannelMessageBatchStore
	batches   [][]string
	calls     int
	countOnly bool
	failID    string
	err       error
}

func (s *countedBatchConversationStore) GetChannelMessagesByIDs(ctx context.Context, scope Scope, conversationID string, ids []string) ([]*ChannelMessage, error) {
	s.calls++
	if !s.countOnly {
		s.batches = append(s.batches, append([]string(nil), ids...))
	}
	for _, id := range ids {
		if id == s.failID && s.err != nil {
			return nil, s.err
		}
	}
	return s.batch.GetChannelMessagesByIDs(ctx, scope, conversationID, ids)
}

func newCountedBatchConversationStore(store ConversationStore) *countedBatchConversationStore {
	return &countedBatchConversationStore{countedPointConversationStore: &countedPointConversationStore{ConversationStore: store}, batch: store.(ChannelMessageBatchStore)}
}

func channelMessageBatchStores(t *testing.T, test func(*testing.T, KernelStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemoryStore()) })
	t.Run("sqlite", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "message-batch.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		test(t, store)
	})
}

func channelMessageBatchConversation(t testing.TB, store ConversationStore, scope Scope, id string) *Conversation {
	t.Helper()
	conversation, _, err := NewConversationService(store).CreateConversation(context.Background(), CreateConversationRequest{
		ID: id, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Batch visibility", IdempotencyKey: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func channelMessageBatchCommit(t testing.TB, store ConversationStore, conversation **Conversation, message *ChannelMessage) {
	t.Helper()
	current := *conversation
	message.Scope, message.ConversationID = current.Scope, current.ID
	message.Sequence, message.CreatedAt, message.IdempotencyKey = current.LastSequence+1, time.Now().UTC(), message.ID
	message.Sender = ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}
	message.Intent, message.Content = MessageIntentUpdate, "Visibility fixture "+message.ID
	if message.Audience.Kind == "" {
		message.Audience.Kind = ConversationAudienceChannel
	}
	next := cloneConversation(current)
	next.Revision, next.LastSequence, next.UpdatedAt = current.Revision+1, message.Sequence, message.CreatedAt
	committed, err := store.CommitChannelMessage(context.Background(), ChannelMessageCommitRecord{Conversation: next, ExpectedRevision: current.Revision, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	*conversation = committed.Conversation
}

func channelMessageBatchHistory(t testing.TB, store KernelStore, count int) (*Conversation, []*AgentRun) {
	t.Helper()
	conversationStore := store.(ConversationStore)
	conversation := channelMessageBatchConversation(t, conversationStore, Scope{Kind: "tenant", ID: "batch-history"}, "history")
	channelMessageBatchCommit(t, conversationStore, &conversation, &ChannelMessage{ID: "parent", Audience: ConversationAudience{Kind: ConversationAudienceRoles, Roles: []string{"reader"}}})
	runs := make([]*AgentRun, 0, count)
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("message-%03d", index)
		channelMessageBatchCommit(t, conversationStore, &conversation, &ChannelMessage{ID: id, ThreadRootID: "parent", ReplyToMessageID: "parent"})
		run, err := NewPortfolioService(store).CreateAgentRun(context.Background(), CreateAgentRunRequest{
			Scope: conversation.Scope, Owner: conversation.Owner, Kind: RunKindConversation, ConcurrencyKey: conversation.ID,
			Goal: "Historical reply", Source: RunSourceChat, Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: id},
		})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	return conversation, runs
}

func TestConversationMessageBatchReducesIdleHistoricalPollReads(t *testing.T) {
	store := NewMemoryStore()
	conversation, runs := channelMessageBatchHistory(t, store, 100)
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	counted := newCountedBatchConversationStore(store)
	changes, err := NewConversationChangeService(counted, store)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Limit: 500, Viewer: &viewer})
	if err != nil || len(initial.Runs) != 100 {
		t.Fatalf("initial historical projection = %#v, %v", initial, err)
	}
	counted.points, counted.batches = 0, nil
	idle, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Cursor: initial.Cursor, Viewer: &viewer})
	if err != nil || idle.HasChanges || counted.points != 0 || len(counted.batches) != 2 || len(counted.batches[0]) != 100 || !reflect.DeepEqual(counted.batches[1], []string{"parent"}) {
		t.Fatalf("idle poll retained historical point reads: changes=%#v err=%v points=%d batches=%v", idle, err, counted.points, counted.batches)
	}
	legacy := &countedPointConversationStore{ConversationStore: store}
	legacyChanges, _ := NewConversationChangeService(legacy, store)
	baseline, err := legacyChanges.filterVisibleRuns(t.Context(), conversation.Scope, conversation.ID, runs, viewer)
	if err != nil || len(baseline) != 100 || legacy.points != 200 {
		t.Fatalf("custom store compatibility changed: visible=%d err=%v reads=%d", len(baseline), err, legacy.points)
	}
	viewer.Roles = nil
	counted.points, counted.batches = 0, nil
	hidden, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Cursor: idle.Cursor, Viewer: &viewer})
	if err != nil || !hidden.RunsChanged || len(hidden.Runs) != 0 || counted.points != 0 || len(counted.batches) != 2 {
		t.Fatalf("poll reused old viewer authorization: changes=%#v err=%v batches=%v", hidden, err, counted.batches)
	}
	viewer.Roles = []string{"reader"}
	restored, err := changes.ListChanges(t.Context(), ConversationChangeRequest{Scope: conversation.Scope, ConversationID: conversation.ID, Cursor: hidden.Cursor, Viewer: &viewer})
	if err != nil || !restored.RunsChanged || len(restored.Runs) != 100 {
		t.Fatalf("poll did not re-evaluate current viewer authorization: %#v, %v", restored, err)
	}
}

func TestConversationMessageBatchPreservesLinkedParentVisibilityAndScope(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, store KernelStore) {
		conversationStore := store.(ConversationStore)
		scope := Scope{Kind: "tenant", ID: "visibility"}
		conversation := channelMessageBatchConversation(t, conversationStore, scope, "channel")
		private := ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "other"}}}
		for _, message := range []*ChannelMessage{
			{ID: "public"}, {ID: "private", Audience: private}, {ID: "parent-with-grandparent", ThreadRootID: "private"},
			{ID: "visible", ThreadRootID: "public", ReplyToMessageID: "public"},
			{ID: "direct-private", Audience: private, ThreadRootID: "must-not-read"},
			{ID: "hidden-thread", ThreadRootID: "private", ReplyToMessageID: "must-not-read"},
			{ID: "hidden-reply", ThreadRootID: "public", ReplyToMessageID: "private"},
			{ID: "missing-parent", ThreadRootID: "missing"}, {ID: "foreign-conversation", ThreadRootID: "foreign-conversation-parent"},
			{ID: "foreign-tenant", ThreadRootID: "foreign-tenant-parent"}, {ID: "self", ThreadRootID: "self", ReplyToMessageID: "self"},
			{ID: "one-level", ThreadRootID: "parent-with-grandparent"},
		} {
			channelMessageBatchCommit(t, conversationStore, &conversation, message)
		}
		foreignConversation := channelMessageBatchConversation(t, conversationStore, scope, "other-channel")
		channelMessageBatchCommit(t, conversationStore, &foreignConversation, &ChannelMessage{ID: "foreign-conversation-parent"})
		foreignTenant := channelMessageBatchConversation(t, conversationStore, Scope{Kind: "tenant", ID: "other-tenant"}, conversation.ID)
		channelMessageBatchCommit(t, conversationStore, &foreignTenant, &ChannelMessage{ID: "foreign-tenant-parent"})
		runs := make([]*AgentRun, 0)
		for _, id := range []string{"visible", "direct-private", "hidden-thread", "hidden-reply", "missing-trigger", "missing-parent", "foreign-conversation", "foreign-tenant", "self", "one-level"} {
			runs = append(runs, &AgentRun{ID: id, Kind: RunKindConversation, RootRunID: id, Context: map[string]interface{}{conversationRunContextTriggerID: id}}, &AgentRun{ID: id + "-child", Kind: RunKindAgentWork, RootRunID: id})
		}
		viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "viewer"}}
		counted := newCountedBatchConversationStore(conversationStore)
		counted.failID, counted.err = "must-not-read", errors.New("hidden parent must not be queried")
		changes, _ := NewConversationChangeService(counted, store)
		visible, err := changes.filterVisibleRuns(t.Context(), scope, conversation.ID, runs, viewer)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(visible))
		for _, run := range visible {
			ids = append(ids, run.ID)
		}
		if expected := []string{"visible", "visible-child", "self", "self-child", "one-level", "one-level-child"}; !reflect.DeepEqual(ids, expected) || counted.points != 0 {
			t.Fatalf("batch parent visibility changed: got=%v points=%d", ids, counted.points)
		}
		legacy := &countedPointConversationStore{ConversationStore: conversationStore, failID: "must-not-read", err: counted.err}
		legacyChanges, _ := NewConversationChangeService(legacy, store)
		baseline, err := legacyChanges.filterVisibleRuns(t.Context(), scope, conversation.ID, runs, viewer)
		if err != nil || !reflect.DeepEqual(visible, baseline) {
			t.Fatalf("batch differs from custom point-read semantics: %#v, %v", baseline, err)
		}
		counted.failID = "public"
		if _, err := changes.filterVisibleRuns(t.Context(), scope, conversation.ID, runs, viewer); !errors.Is(err, counted.err) {
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
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "exact-batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation := channelMessageBatchConversation(t, store, scope, "channel")
	channelMessageBatchCommit(t, store, &conversation, &ChannelMessage{ID: "shared"})
	otherConversation := channelMessageBatchConversation(t, store, scope, "other")
	channelMessageBatchCommit(t, store, &otherConversation, &ChannelMessage{ID: "shared"})
	otherTenant := channelMessageBatchConversation(t, store, Scope{Kind: "tenant", ID: "two"}, "channel")
	channelMessageBatchCommit(t, store, &otherTenant, &ChannelMessage{ID: "shared"})
	reader := &countedChannelMessageSQLReader{db: store.db}
	selected, err := getChannelMessageBatchSQL(t.Context(), reader, "channel_messages", scope, conversation.ID, []string{"shared", "missing", "shared"}, false)
	if err != nil || reader.queries != 1 || len(selected) != 1 || selected[0].Scope != scope || selected[0].ConversationID != conversation.ID {
		t.Fatalf("exact scoped batch query = %#v, %v; reads=%d", selected, err, reader.queries)
	}
	selected[0].Content = "caller changed its decoded message"
	fresh, err := store.GetChannelMessagesByIDs(t.Context(), scope, conversation.ID, []string{"shared"})
	if err != nil || len(fresh) != 1 || fresh[0].Content == selected[0].Content {
		t.Fatalf("caller mutation affected a later batch: %#v, %v", fresh, err)
	}
	if _, err := store.GetChannelMessagesByIDs(t.Context(), scope, conversation.ID, make([]string, MaximumChannelMessageBatchSize+1)); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("unbounded SQL batch accepted: %v", err)
	}
	reader.queries = 0
	empty, err := getChannelMessageBatchSQL(t.Context(), reader, "channel_messages", scope, conversation.ID, nil, false)
	if err != nil || len(empty) != 0 || reader.queries != 0 {
		t.Fatalf("empty batch performed a database read: %#v %v %d", empty, err, reader.queries)
	}
}

func TestConversationMessageBatchChunksAndDeduplicatesExactIDs(t *testing.T) {
	store := NewMemoryStore()
	conversation, runs := channelMessageBatchHistory(t, store, 401)
	runs = append(runs, runs[0])
	counted := newCountedBatchConversationStore(store)
	changes, _ := NewConversationChangeService(counted, store)
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	visible, err := changes.filterVisibleRuns(t.Context(), conversation.Scope, conversation.ID, runs, viewer)
	if err != nil || len(visible) != len(runs) || len(counted.batches) != 4 {
		t.Fatalf("chunked visibility = %d, %v; batch sizes=%v", len(visible), err, counted.batches)
	}
	for index, size := range []int{200, 200, 1, 1} {
		if len(counted.batches[index]) != size {
			t.Fatalf("chunk%d has%d IDs, expected%d", index, len(counted.batches[index]), size)
		}
	}
}

func BenchmarkConversationMessageVisibilityHistory(b *testing.B) {
	store, err := NewSQLiteStore(filepath.Join(b.TempDir(), "poll-batch.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	conversation, runs := channelMessageBatchHistory(b, store, 100)
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "viewer"}, Roles: []string{"reader"}}
	for _, batched := range []bool{false, true} {
		name := "point"
		if batched {
			name = "batch"
		}
		b.Run(name, func(b *testing.B) {
			points := &countedPointConversationStore{ConversationStore: store}
			var conversationStore ConversationStore = points
			var batch *countedBatchConversationStore
			if batched {
				batch = &countedBatchConversationStore{countedPointConversationStore: points, batch: store, countOnly: true}
				conversationStore = batch
			}
			changes, _ := NewConversationChangeService(conversationStore, store)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if visible, err := changes.filterVisibleRuns(context.Background(), conversation.Scope, conversation.ID, runs, viewer); err != nil || len(visible) != 100 {
					b.Fatalf("historical visibility = %d, %v", len(visible), err)
				}
			}
			reads := points.points
			if batch != nil {
				reads += batch.calls
			}
			b.ReportMetric(float64(reads)/float64(b.N), "message_reads/op")
		})
	}
}
