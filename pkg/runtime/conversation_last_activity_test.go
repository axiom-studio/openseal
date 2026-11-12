package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
)

// LastConversationActivity answers what listing each owner's active
// conversations and taking the newest one with a message would.
func TestLastConversationActivityMatchesTheOwnerList(t *testing.T) {
	for name, open := range conversationOriginFilterStores() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()
			service := runtimeconversation.NewConversationService(store)
			scope := models.Scope{Kind: "tenant", ID: "last-activity"}
			conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
			conversationtest.SeedSpace(t, store.(models.SpaceStore), models.Scope{Kind: "tenant", ID: "elsewhere"})
			person := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "ana"}
			create := func(scope models.Scope, ownerType models.OwnerType, ownerID, key string, post bool) *models.Conversation {
				t.Helper()
				conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
					Scope: scope, Owner: models.ObjectiveOwner{Type: ownerType, ID: ownerID}, Title: key, IdempotencyKey: key,
				})
				if err != nil {
					t.Fatal(err)
				}
				if post {
					posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
						Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: person,
						Intent: models.MessageIntentUpdate, Content: "Hi", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key + "-1",
					})
					if err != nil {
						t.Fatal(err)
					}
					conversation = posted.Conversation
				}
				// Distinct updated_at values keep the expected order independent of clock resolution.
				time.Sleep(2 * time.Millisecond)
				return conversation
			}
			create(scope, models.OwnerTypeAgent, "scout", "scout-old", true)
			create(scope, models.OwnerTypeAgent, "scout", "scout-new", true)
			create(scope, models.OwnerTypeAgent, "scout", "scout-empty", false)
			archived := create(scope, models.OwnerTypeAgent, "scout", "scout-archived", true)
			status := models.ConversationStatusArchived
			if _, err := service.UpdateConversation(ctx, runtimeconversation.UpdateConversationRequest{
				Scope: scope, ConversationID: archived.ID, ExpectedRevision: archived.Revision, Status: &status,
			}); err != nil {
				t.Fatal(err)
			}
			create(scope, models.OwnerTypeAgent, "quiet", "quiet-empty", false)
			create(scope, models.OwnerTypeAgent, "press", "press-only", true)
			create(scope, models.OwnerTypeAgent, "unasked", "unasked", true)
			create(scope, models.OwnerTypeTeam, "scout", "team-scout", true)
			create(models.Scope{Kind: "tenant", ID: "elsewhere"}, models.OwnerTypeAgent, "scout", "scout-elsewhere", true)

			asked := []string{"scout", "quiet", "press", "nobody"}
			got, err := store.LastConversationActivity(ctx, scope, models.OwnerTypeAgent, asked)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]time.Time{}
			for _, id := range asked {
				listed, err := store.ListConversations(ctx, models.ConversationFilter{
					Scope: scope, Owner: &models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: id}, Statuses: []models.ConversationStatus{models.ConversationStatusActive}, Limit: 100,
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, conversation := range listed {
					if conversation.LastSequence > 0 {
						want[id] = conversation.UpdatedAt
						break
					}
				}
			}
			if len(want) != 2 || len(got) != len(want) {
				t.Fatalf("last activity = %v, want %v", got, want)
			}
			for id, at := range want {
				if got[id].Format(time.RFC3339Nano) != at.Format(time.RFC3339Nano) {
					t.Fatalf("%s last activity = %s, want %s", id, got[id].Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
				}
			}
			if none, err := store.LastConversationActivity(ctx, scope, models.OwnerTypeAgent, nil); err != nil || len(none) != 0 {
				t.Fatalf("no owners = %v, %v", none, err)
			}
		})
	}
}
