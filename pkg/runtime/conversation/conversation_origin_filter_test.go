package conversation_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func conversationOriginFilterStores() map[string]func(*testing.T) models.ConversationStore {
	return map[string]func(*testing.T) models.ConversationStore{
		"sqlite": func(t *testing.T) models.ConversationStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "origin-filter.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
		"postgres": func(t *testing.T) models.ConversationStore {
			pgSchema := storetest.NewPostgresSchema(t, "openseal_origin_filter_")
			dsn := pgSchema.DSN
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			store, err := postgres.New(ctx, dsn, postgres.WithSchema(pgSchema.Name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = store.Close()
			})
			return store
		},
	}
}

func conversationIDs(conversations []*models.Conversation) []string {
	ids := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		ids = append(ids, conversation.ID)
	}
	return ids
}

func TestConversationOriginFilter(t *testing.T) {
	for name, open := range conversationOriginFilterStores() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()
			service := conversation.NewConversationService(store)
			scope := models.Scope{Kind: "tenant", ID: "origin-filter"}
			builder := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"}
			other := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "helper"}
			conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
			create := func(key string, owner models.ObjectiveOwner, origin *models.ConversationReference) {
				t.Helper()
				if _, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{
					Scope: scope, Owner: owner, Title: key, Origin: origin, IdempotencyKey: key,
				}); err != nil {
					t.Fatal(err)
				}
				// Distinct updated_at values keep the expected order independent of clock resolution.
				time.Sleep(2 * time.Millisecond)
			}
			builderOrigin := func(key string) *models.ConversationReference {
				return &models.ConversationReference{Kind: models.ConversationReferenceAgentBuilder, ID: key}
			}
			create("plain", builder, nil)
			create("build-1", builder, builderOrigin("build-1"))
			create("embed", builder, &models.ConversationReference{Kind: models.ConversationReferenceEmbedSession, ID: "embed"})
			create("build-2", builder, builderOrigin("build-2"))
			create("other-owner-build", other, builderOrigin("other-owner-build"))
			create("build-3", builder, builderOrigin("build-3"))

			all, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, Owner: &builder})
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 5 {
				t.Fatalf("unfiltered = %v", conversationIDs(all))
			}
			want := make([]string, 0, 3)
			for _, conversation := range all {
				if conversation.Origin != nil && conversation.Origin.Kind == models.ConversationReferenceAgentBuilder {
					want = append(want, conversation.ID)
				}
			}
			if len(want) != 3 {
				t.Fatalf("builder conversations in unfiltered list = %v", want)
			}

			filtered, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, Owner: &builder, OriginKind: models.ConversationReferenceAgentBuilder})
			if err != nil {
				t.Fatal(err)
			}
			if got := conversationIDs(filtered); !reflect.DeepEqual(got, want) {
				t.Fatalf("filtered = %v, want %v (unfiltered order)", got, want)
			}
			for _, conversation := range filtered {
				if conversation.Origin == nil || conversation.Origin.Kind != models.ConversationReferenceAgentBuilder || conversation.Owner != builder {
					t.Fatalf("filtered conversation %s = origin %#v owner %#v", conversation.ID, conversation.Origin, conversation.Owner)
				}
			}

			for offset := 0; offset <= 3; offset++ {
				page, err := store.ListConversations(ctx, models.ConversationFilter{
					Scope: scope, Owner: &builder, OriginKind: models.ConversationReferenceAgentBuilder, Limit: 2, Offset: offset,
				})
				if err != nil {
					t.Fatal(err)
				}
				end := offset + 2
				if end > len(want) {
					end = len(want)
				}
				if got := conversationIDs(page); !reflect.DeepEqual(got, append([]string{}, want[offset:end]...)) {
					t.Fatalf("page offset %d = %v, want %v", offset, got, want[offset:end])
				}
			}

			anyOwner, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, OriginKind: models.ConversationReferenceAgentBuilder})
			if err != nil {
				t.Fatal(err)
			}
			if len(anyOwner) != 4 {
				t.Fatalf("builder conversations across owners = %v", conversationIDs(anyOwner))
			}

			none, err := store.ListConversations(ctx, models.ConversationFilter{
				Scope: scope, Owner: &builder, OriginKind: models.ConversationReferenceAgentBuilder, Statuses: []models.ConversationStatus{models.ConversationStatusArchived},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(none) != 0 {
				t.Fatalf("archived builder conversations = %v", conversationIDs(none))
			}

			empty, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, Owner: &builder, OriginKind: ""})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(conversationIDs(empty), conversationIDs(all)) {
				t.Fatalf("empty origin kind = %v, want %v", conversationIDs(empty), conversationIDs(all))
			}
			objectives, err := store.ListConversations(ctx, models.ConversationFilter{Scope: scope, OriginKind: models.ConversationReferenceObjective})
			if err != nil || len(objectives) != 0 {
				t.Fatalf("objective conversations = %v, %v", conversationIDs(objectives), err)
			}
		})
	}
}
