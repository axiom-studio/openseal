package runtime

import (
	"context"
	"testing"
	"time"
)

// LastConversationActivity answers what listing each owner's active
// conversations and taking the newest one with a message would.
func TestLastConversationActivityMatchesTheOwnerList(t *testing.T) {
	for name, open := range conversationOriginFilterStores() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "last-activity"}
			seedTestSpace(t, store.(SpaceStore), scope)
			seedTestSpace(t, store.(SpaceStore), Scope{Kind: "tenant", ID: "elsewhere"})
			person := ConversationParticipant{Type: ConversationParticipantUser, ID: "ana"}
			create := func(scope Scope, ownerType OwnerType, ownerID, key string, post bool) *Conversation {
				t.Helper()
				conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
					Scope: scope, Owner: ObjectiveOwner{Type: ownerType, ID: ownerID}, Title: key, IdempotencyKey: key,
				})
				if err != nil {
					t.Fatal(err)
				}
				if post {
					posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
						Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: person,
						Intent: MessageIntentUpdate, Content: "Hi", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: key + "-1",
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
			create(scope, OwnerTypeAgent, "scout", "scout-old", true)
			create(scope, OwnerTypeAgent, "scout", "scout-new", true)
			create(scope, OwnerTypeAgent, "scout", "scout-empty", false)
			archived := create(scope, OwnerTypeAgent, "scout", "scout-archived", true)
			status := ConversationStatusArchived
			if _, err := service.UpdateConversation(ctx, UpdateConversationRequest{
				Scope: scope, ConversationID: archived.ID, ExpectedRevision: archived.Revision, Status: &status,
			}); err != nil {
				t.Fatal(err)
			}
			create(scope, OwnerTypeAgent, "quiet", "quiet-empty", false)
			create(scope, OwnerTypeAgent, "press", "press-only", true)
			create(scope, OwnerTypeAgent, "unasked", "unasked", true)
			create(scope, OwnerTypeTeam, "scout", "team-scout", true)
			create(Scope{Kind: "tenant", ID: "elsewhere"}, OwnerTypeAgent, "scout", "scout-elsewhere", true)

			asked := []string{"scout", "quiet", "press", "nobody"}
			got, err := store.LastConversationActivity(ctx, scope, OwnerTypeAgent, asked)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]time.Time{}
			for _, id := range asked {
				listed, err := store.ListConversations(ctx, ConversationFilter{
					Scope: scope, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: id}, Statuses: []ConversationStatus{ConversationStatusActive}, Limit: 100,
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
			if none, err := store.LastConversationActivity(ctx, scope, OwnerTypeAgent, nil); err != nil || len(none) != 0 {
				t.Fatalf("no owners = %v, %v", none, err)
			}
		})
	}
}
