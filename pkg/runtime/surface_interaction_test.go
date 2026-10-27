package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func surfaceInteractionStores(t *testing.T) []struct {
	name  string
	store func(*testing.T) KernelStore
} {
	return []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "surface-interactions.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
		{name: "postgres", store: func(t *testing.T) KernelStore {
			_, store := newSurfaceInteractionPostgresStore(t)
			return store
		}},
	}
}

func newSurfaceInteractionPostgresStore(t *testing.T) (context.Context, *PostgresStore) {
	t.Helper()
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("openseal_surface_interactions_"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	return ctx, store
}

type surfaceInteractionFixture struct {
	ctx           context.Context
	scope         Scope
	conversation  *Conversation
	conversations *ConversationService
}

func newSurfaceInteractionFixture(t *testing.T, store KernelStore) surfaceInteractionFixture {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "cards"}
	conversations := NewConversationService(store.(ConversationStore))
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	conversations.now = func() time.Time { now = now.Add(time.Second); return now }
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "family"}, Title: "Trip", IdempotencyKey: "trip",
	})
	if err != nil {
		t.Fatal(err)
	}
	return surfaceInteractionFixture{ctx: ctx, scope: scope, conversation: conversation, conversations: conversations}
}

// postCard posts (or replays) the message that carries a surface's card and
// returns its id.
func (f surfaceInteractionFixture) postCard(t *testing.T, surfaceID string, audience ConversationAudience) string {
	t.Helper()
	conversation, err := f.conversations.GetConversation(f.ctx, f.scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := f.conversations.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.scope, ConversationID: f.conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "finn"}, Intent: MessageIntentUpdate,
		Content: "Card " + surfaceID, Audience: audience, IdempotencyKey: "card-" + surfaceID,
		References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "artifact-" + surfaceID, Version: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return posted.Message.ID
}

// record fills test defaults: version 1 and the surface's channel card message.
func (f surfaceInteractionFixture) record(t *testing.T, req RecordSurfaceInteractionRequest) (*SurfaceInteraction, bool, error) {
	t.Helper()
	req.Scope, req.ConversationID = f.scope, f.conversation.ID
	if req.SurfaceArtifactID == "" {
		req.SurfaceArtifactID = "artifact-" + req.SurfaceID
	}
	if req.SurfaceVersion == 0 {
		req.SurfaceVersion = 1
	}
	if req.MessageID == "" {
		req.MessageID = f.postCard(t, req.SurfaceID, ConversationAudience{Kind: ConversationAudienceChannel})
	}
	return f.conversations.RecordSurfaceInteraction(f.ctx, req)
}

func surfaceInteractionViewer(id string) *ConversationViewer {
	return &ConversationViewer{Participant: surfaceInteractionUser(id)}
}

func surfaceInteractionUser(id string) ConversationParticipant {
	return ConversationParticipant{Type: ConversationParticipantUser, ID: id}
}

func sameJSON(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var left, right interface{}
	if err := json.Unmarshal(got, &left); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &right); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(left, right)
}

func surfaceInteractionKeys(interactions []*SurfaceInteraction) []string {
	keys := make([]string, 0, len(interactions))
	for _, interaction := range interactions {
		keys = append(keys, interaction.IdempotencyKey)
	}
	return keys
}

func TestSurfaceInteractionSingleUseIsUniquePerVersionComponentAndActor(t *testing.T) {
	for _, fixture := range surfaceInteractionStores(t) {
		t.Run(fixture.name, func(t *testing.T) {
			f := newSurfaceInteractionFixture(t, fixture.store(t))
			quiz := RecordSurfaceInteractionRequest{
				SurfaceID: "biology-quiz", ComponentID: "q1", Actor: surfaceInteractionUser("ana"),
				Values: json.RawMessage(`{"choice":"mitochondria"}`), Outcome: json.RawMessage(`{"correct":true}`), IdempotencyKey: "ana-q1",
			}
			first, replayed, err := f.record(t, quiz)
			if err != nil || replayed || first.Sequence != 1 || !sameJSON(t, first.Values, `{"choice":"mitochondria"}`) || !sameJSON(t, first.Outcome, `{"correct":true}`) {
				t.Fatalf("first = %#v, %t, %v", first, replayed, err)
			}
			// A client retry with the same key replays the stored answer.
			retry := quiz
			retry.Values = json.RawMessage(`{"choice":"ribosome"}`)
			again, replayed, err := f.record(t, retry)
			if err != nil || !replayed || again.ID != first.ID || again.Sequence != 1 || !sameJSON(t, again.Values, `{"choice":"mitochondria"}`) {
				t.Fatalf("replay = %#v, %t, %v", again, replayed, err)
			}
			// A second answer under a new key is a conflict carrying the first.
			second := quiz
			second.IdempotencyKey, second.Values = "ana-q1-again", json.RawMessage(`{"choice":"ribosome"}`)
			_, _, err = f.record(t, second)
			var conflict *SurfaceInteractionConflictError
			if !errors.Is(err, ErrSurfaceInteractionConflict) || !errors.As(err, &conflict) || conflict.Existing == nil || conflict.Existing.ID != first.ID {
				t.Fatalf("duplicate = %v", err)
			}
			// The same key cannot be reused for another component.
			reused := quiz
			reused.ComponentID = "q2"
			if _, _, err := f.record(t, reused); !errors.As(err, &conflict) || conflict.Existing.ID != first.ID {
				t.Fatalf("reused key = %v", err)
			}
			// Other actors, components and versions answer independently.
			for _, other := range []RecordSurfaceInteractionRequest{
				{SurfaceID: "biology-quiz", ComponentID: "q1", Actor: surfaceInteractionUser("ben"), Values: json.RawMessage(`{}`), IdempotencyKey: "ben-q1"},
				{SurfaceID: "biology-quiz", ComponentID: "q2", Actor: surfaceInteractionUser("ana"), Values: json.RawMessage(`{}`), IdempotencyKey: "ana-q2"},
				{SurfaceID: "biology-quiz", SurfaceVersion: 2, ComponentID: "q1", Actor: surfaceInteractionUser("ana"), Values: json.RawMessage(`{}`), IdempotencyKey: "ana-q1-v2"},
				{SurfaceID: "biology-quiz", ComponentID: "q1", Actor: ConversationParticipant{Type: ConversationParticipantAgent, ID: "ana"}, Values: json.RawMessage(`{}`), IdempotencyKey: "agent-q1"},
			} {
				if _, replayed, err := f.record(t, other); err != nil || replayed {
					t.Fatalf("%s = %t, %v", other.IdempotencyKey, replayed, err)
				}
			}
			listed, err := f.conversations.ListSurfaceInteractions(f.ctx, SurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID})
			if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(listed), []string{"ana-q1", "ben-q1", "ana-q2", "ana-q1-v2", "agent-q1"}) {
				t.Fatalf("listed = %v, %v", surfaceInteractionKeys(listed), err)
			}
			for index, interaction := range listed {
				if interaction.Sequence != int64(index+1) {
					t.Fatalf("sequence %d = %d", index, interaction.Sequence)
				}
			}
		})
	}
}

func TestSurfaceInteractionRepeatablesDedupeOnKeyAndResolveLastWritePerItem(t *testing.T) {
	for _, fixture := range surfaceInteractionStores(t) {
		t.Run(fixture.name, func(t *testing.T) {
			f := newSurfaceInteractionFixture(t, fixture.store(t))
			tick := func(actor, item, key string, checked bool) RecordSurfaceInteractionRequest {
				values, _ := json.Marshal(map[string]bool{"checked": checked})
				return RecordSurfaceInteractionRequest{
					SurfaceID: "packing", ComponentID: "list", ItemKey: item, Actor: surfaceInteractionUser(actor), Repeatable: true, Values: values, IdempotencyKey: key,
				}
			}
			for _, req := range []RecordSurfaceInteractionRequest{
				tick("ana", "passport", "k1", true),
				tick("ana", "passport", "k2", false),
				tick("ben", "socks", "k3", true),
				tick("ben", "passport", "k4", true),
			} {
				if _, replayed, err := f.record(t, req); err != nil || replayed {
					t.Fatalf("%s = %t, %v", req.IdempotencyKey, replayed, err)
				}
			}
			replay, replayed, err := f.record(t, tick("ana", "passport", "k1", true))
			if err != nil || !replayed || replay.Sequence != 1 {
				t.Fatalf("replay = %#v, %t, %v", replay, replayed, err)
			}
			// Another surface in the conversation stays out of this surface's state.
			if _, _, err := f.record(t, RecordSurfaceInteractionRequest{SurfaceID: "chips", ComponentID: "next", Actor: surfaceInteractionUser("ana"), Repeatable: true,
				Values: json.RawMessage(`{"chip":"weather"}`), IdempotencyKey: "k5"}); err != nil {
				t.Fatal(err)
			}

			perActor, err := f.conversations.ListLatestSurfaceInteractions(f.ctx, LatestSurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, SurfaceID: "packing"})
			if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(perActor), []string{"k2", "k3", "k4"}) || !sameJSON(t, perActor[0].Values, `{"checked":false}`) {
				t.Fatalf("per actor = %v, %v", surfaceInteractionKeys(perActor), err)
			}
			shared, err := f.conversations.ListLatestSurfaceInteractions(f.ctx, LatestSurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, SurfaceID: "packing", AcrossActors: true})
			if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(shared), []string{"k3", "k4"}) {
				t.Fatalf("shared = %v, %v", surfaceInteractionKeys(shared), err)
			}
			bySurface, err := f.conversations.ListSurfaceInteractions(f.ctx, SurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, SurfaceID: "packing", AfterSequence: 1, Limit: 2})
			if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(bySurface), []string{"k2", "k3"}) {
				t.Fatalf("by surface = %v, %v", surfaceInteractionKeys(bySurface), err)
			}

			invalid := tick("ana", "passport", "k6", true)
			invalid.Repeatable = false
			if _, _, err := f.record(t, invalid); !errors.Is(err, ErrInvalidSurfaceInteraction) {
				t.Fatalf("single-use item key = %v", err)
			}
			invalid = tick("ana", "passport", "k6", true)
			invalid.Values = json.RawMessage(`{`)
			if _, _, err := f.record(t, invalid); !errors.Is(err, ErrInvalidSurfaceInteraction) {
				t.Fatalf("invalid values = %v", err)
			}
			if _, _, err := f.conversations.RecordSurfaceInteraction(f.ctx, RecordSurfaceInteractionRequest{Scope: f.scope, ConversationID: "missing",
				SurfaceArtifactID: "a", SurfaceID: "s", SurfaceVersion: 1, ComponentID: "c", Actor: surfaceInteractionUser("ana"), Values: json.RawMessage(`{}`), IdempotencyKey: "k7",
			}); !errors.Is(err, ErrConversationNotFound) {
				t.Fatalf("missing conversation = %v", err)
			}
		})
	}
}

func TestConversationChangesCarrySurfaceInteractions(t *testing.T) {
	for _, fixture := range surfaceInteractionStores(t) {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			f := newSurfaceInteractionFixture(t, store)
			changes, err := NewConversationChangeService(store.(ConversationStore), store)
			if err != nil {
				t.Fatal(err)
			}
			list := func(cursor string, limit int) *ConversationChangeSet {
				t.Helper()
				set, err := changes.ListChanges(f.ctx, ConversationChangeRequest{Scope: f.scope, ConversationID: f.conversation.ID, Cursor: cursor, Limit: limit})
				if err != nil {
					t.Fatal(err)
				}
				return set
			}
			initial := list("", 0)
			if !initial.SurfaceInteractionsChanged || initial.SurfaceInteractions == nil || len(initial.SurfaceInteractions) != 0 {
				t.Fatalf("initial = %#v", initial)
			}
			encoded, err := json.Marshal(initial)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["surfaceInteractions"]) != "[]" || string(fields["surfaceInteractionsChanged"]) != "true" {
				t.Fatalf("JSON fields = %s", encoded)
			}
			for _, key := range []string{"vote-ana", "vote-ben", "vote-cy"} {
				if _, _, err := f.record(t, RecordSurfaceInteractionRequest{SurfaceID: "dinner-poll", ComponentID: "poll", ItemKey: "tacos",
					Actor: surfaceInteractionUser(key), Repeatable: true, Values: json.RawMessage(`{"selected":true}`), IdempotencyKey: key}); err != nil {
					t.Fatal(err)
				}
			}
			page := list(initial.Cursor, 2)
			if !page.HasChanges || !page.HasMore || !page.SurfaceInteractionsChanged || !reflect.DeepEqual(surfaceInteractionKeys(page.SurfaceInteractions), []string{"vote-ana", "vote-ben"}) {
				t.Fatalf("page = %#v", page)
			}
			rest := list(page.Cursor, 2)
			if !rest.HasChanges || rest.HasMore || !reflect.DeepEqual(surfaceInteractionKeys(rest.SurfaceInteractions), []string{"vote-cy"}) {
				t.Fatalf("rest = %#v", rest)
			}
			unchanged := list(rest.Cursor, 2)
			if unchanged.HasChanges || unchanged.SurfaceInteractionsChanged || len(unchanged.SurfaceInteractions) != 0 || unchanged.Cursor != rest.Cursor {
				t.Fatalf("unchanged = %#v", unchanged)
			}
			// A fresh observer receives every interaction on its initial page.
			if fresh := list("", 0); len(fresh.SurfaceInteractions) != 3 {
				t.Fatalf("fresh = %v", surfaceInteractionKeys(fresh.SurfaceInteractions))
			}
		})
	}
}

func TestPostgresSurfaceInteractionMigrationRollsBackAndReapplies(t *testing.T) {
	ctx, store := newSurfaceInteractionPostgresStore(t)
	exists := func() bool {
		var exists bool
		if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, store.schema+".surface_interactions").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != surfaceInteractionMigrationVersion || !exists() {
		t.Fatalf("schema version = %d, %v; table %t", version, err, exists())
	}
	if err := store.RollbackPostgresMigrations(ctx, surfaceInteractionMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != surfaceInteractionMigrationVersion-1 || exists() {
		t.Fatalf("after rollback: version %d, %v; table %t", version, err, exists())
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != surfaceInteractionMigrationVersion || !exists() {
		t.Fatalf("after reapply: version %d, %v; table %t", version, err, exists())
	}
}

func TestSurfaceInteractionVisibilityFollowsCardMessage(t *testing.T) {
	for _, fixture := range surfaceInteractionStores(t) {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			f := newSurfaceInteractionFixture(t, store)
			answer := func(actor, surface, key string, repeatable bool) RecordSurfaceInteractionRequest {
				req := RecordSurfaceInteractionRequest{SurfaceID: surface, ComponentID: "c1", Actor: surfaceInteractionUser(actor),
					Repeatable: repeatable, Values: json.RawMessage(`{"pick":1}`), IdempotencyKey: key}
				if repeatable {
					req.ItemKey = "tacos"
				}
				return req
			}
			// Only ana can see the "secret" card's message.
			secretMessage := f.postCard(t, "secret", ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{surfaceInteractionUser("ana")}})
			secret := answer("ana", "secret", "secret-ana", true)
			secret.MessageID = secretMessage
			for _, req := range []RecordSurfaceInteractionRequest{
				answer("ben", "quiz", "ben-q1", false),
				answer("ana", "quiz", "ana-q1", false),
				answer("ana", "poll", "vote-ana", true),
				answer("ben", "poll", "vote-ben", true),
				secret,
			} {
				if _, _, err := f.record(t, req); err != nil {
					t.Fatalf("%s: %v", req.IdempotencyKey, err)
				}
			}
			want := map[string][]string{
				"":    {"ben-q1", "ana-q1", "vote-ana", "vote-ben", "secret-ana"},
				"ana": {"ben-q1", "ana-q1", "vote-ana", "vote-ben", "secret-ana"},
				"ben": {"ben-q1", "ana-q1", "vote-ana", "vote-ben"},
				"cy":  {"ben-q1", "ana-q1", "vote-ana", "vote-ben"},
			}
			changes, err := NewConversationChangeService(store.(ConversationStore), store)
			if err != nil {
				t.Fatal(err)
			}
			for viewerID, keys := range want {
				var viewer *ConversationViewer
				if viewerID != "" {
					viewer = surfaceInteractionViewer(viewerID)
				}
				set, err := changes.ListChanges(f.ctx, ConversationChangeRequest{Scope: f.scope, ConversationID: f.conversation.ID, Viewer: viewer})
				if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(set.SurfaceInteractions), keys) {
					t.Fatalf("changes for %q = %v, %v", viewerID, surfaceInteractionKeys(set.SurfaceInteractions), err)
				}
				listed, err := f.conversations.ListSurfaceInteractions(f.ctx, SurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, Viewer: viewer})
				if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(listed), keys) {
					t.Fatalf("list for %q = %v, %v", viewerID, surfaceInteractionKeys(listed), err)
				}
			}

			latest := func(surface string, viewer *ConversationViewer, acrossActors bool) []string {
				t.Helper()
				result, err := f.conversations.ListLatestSurfaceInteractions(f.ctx, LatestSurfaceInteractionFilter{
					Scope: f.scope, ConversationID: f.conversation.ID, SurfaceID: surface, AcrossActors: acrossActors, Viewer: viewer,
				})
				if err != nil {
					t.Fatal(err)
				}
				return surfaceInteractionKeys(result)
			}
			// Everyone who sees a card's message sees all its interactions.
			if got := latest("quiz", nil, true); !reflect.DeepEqual(got, []string{"ana-q1"}) {
				t.Fatalf("quiz across actors = %v", got)
			}
			if got := latest("quiz", surfaceInteractionViewer("ben"), true); !reflect.DeepEqual(got, []string{"ana-q1"}) {
				t.Fatalf("ben quiz across actors = %v", got)
			}
			if got := latest("quiz", surfaceInteractionViewer("cy"), false); !reflect.DeepEqual(got, []string{"ben-q1", "ana-q1"}) {
				t.Fatalf("cy quiz = %v", got)
			}
			if got := latest("poll", surfaceInteractionViewer("cy"), true); !reflect.DeepEqual(got, []string{"vote-ben"}) {
				t.Fatalf("cy poll across actors = %v", got)
			}
			if got := latest("poll", surfaceInteractionViewer("cy"), false); !reflect.DeepEqual(got, []string{"vote-ana", "vote-ben"}) {
				t.Fatalf("cy poll per actor = %v", got)
			}
			if got := latest("secret", surfaceInteractionViewer("ben"), true); len(got) != 0 {
				t.Fatalf("ben secret = %v", got)
			}
			if got := latest("secret", surfaceInteractionViewer("ana"), true); !reflect.DeepEqual(got, []string{"secret-ana"}) {
				t.Fatalf("ana secret = %v", got)
			}

			// Stores never filter by viewer themselves.
			if _, err := store.(ConversationStore).ListSurfaceInteractions(f.ctx, SurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, Viewer: surfaceInteractionViewer("ana")}); !errors.Is(err, ErrInvalidSurfaceInteraction) {
				t.Fatalf("store viewer = %v", err)
			}
			// The message must carry the surface.
			wrongMessage := answer("ana", "poll", "wrong-message", true)
			wrongMessage.MessageID = secretMessage
			if _, _, err := f.record(t, wrongMessage); !errors.Is(err, ErrInvalidSurfaceInteraction) {
				t.Fatalf("message without surface = %v", err)
			}
			missingMessage := answer("ana", "poll", "missing-message", true)
			missingMessage.MessageID = "missing"
			if _, _, err := f.record(t, missingMessage); !errors.Is(err, ErrChannelMessageNotFound) {
				t.Fatalf("missing message = %v", err)
			}
		})
	}
}

func TestConversationChangesAdvancePastHiddenSurfaceInteractions(t *testing.T) {
	for _, fixture := range surfaceInteractionStores(t) {
		t.Run(fixture.name, func(t *testing.T) {
			store := fixture.store(t)
			f := newSurfaceInteractionFixture(t, store)
			changes, err := NewConversationChangeService(store.(ConversationStore), store)
			if err != nil {
				t.Fatal(err)
			}
			f.postCard(t, "quiz", ConversationAudience{Kind: ConversationAudienceChannel})
			// ana's card goes only to ana, so ben never sees its interactions.
			anaCard := f.postCard(t, "private", ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{surfaceInteractionUser("ana")}})
			viewer := surfaceInteractionViewer("ben")
			list := func(cursor string, limit int) *ConversationChangeSet {
				t.Helper()
				set, err := changes.ListChanges(f.ctx, ConversationChangeRequest{Scope: f.scope, ConversationID: f.conversation.ID, Cursor: cursor, Limit: limit, Viewer: viewer})
				if err != nil {
					t.Fatal(err)
				}
				return set
			}
			initial := list("", 0)
			for index, req := range []struct{ actor, key string }{{"ana", "a1"}, {"ana", "a2"}, {"ben", "b1"}, {"ana", "a3"}, {"ben", "b2"}} {
				surface, message := "quiz", ""
				if req.actor == "ana" {
					surface, message = "private", anaCard
				}
				if _, _, err := f.record(t, RecordSurfaceInteractionRequest{SurfaceID: surface, ComponentID: "q" + string(rune('1'+index)), MessageID: message,
					Actor: surfaceInteractionUser(req.actor), Values: json.RawMessage(`{}`), IdempotencyKey: req.key}); err != nil {
					t.Fatal(err)
				}
			}
			hidden := list(initial.Cursor, 2)
			if len(hidden.SurfaceInteractions) != 0 || !hidden.HasMore || hidden.Cursor == initial.Cursor || hidden.SurfaceInteractionsChanged {
				t.Fatalf("hidden page = %#v", hidden)
			}
			middle := list(hidden.Cursor, 2)
			if !reflect.DeepEqual(surfaceInteractionKeys(middle.SurfaceInteractions), []string{"b1"}) || !middle.HasMore || !middle.HasChanges {
				t.Fatalf("middle page = %v, %#v", surfaceInteractionKeys(middle.SurfaceInteractions), middle)
			}
			last := list(middle.Cursor, 2)
			if !reflect.DeepEqual(surfaceInteractionKeys(last.SurfaceInteractions), []string{"b2"}) || last.HasMore {
				t.Fatalf("last page = %v, %#v", surfaceInteractionKeys(last.SurfaceInteractions), last)
			}
			if done := list(last.Cursor, 2); len(done.SurfaceInteractions) != 0 || done.HasMore || done.Cursor != last.Cursor {
				t.Fatalf("done = %#v", done)
			}
			// The service list scans through hidden pages to fill its page.
			listed, err := f.conversations.ListSurfaceInteractions(f.ctx, SurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, Limit: 1, Viewer: viewer})
			if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(listed), []string{"b1"}) {
				t.Fatalf("listed = %v, %v", surfaceInteractionKeys(listed), err)
			}
			listed, err = f.conversations.ListSurfaceInteractions(f.ctx, SurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, AfterSequence: 3, Limit: 1, Viewer: viewer})
			if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(listed), []string{"b2"}) {
				t.Fatalf("listed after b1 = %v, %v", surfaceInteractionKeys(listed), err)
			}
		})
	}
}
