package conversationtest

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// ThreadStartStore is a kernel store that also stores conversations.
type ThreadStartStore interface {
	models.KernelStore
	models.ConversationStore
}

// ForThreadStartStores runs test on a SQLite and a Postgres store, with a
// function that opens the same database again.
func ForThreadStartStores(t *testing.T, test func(*testing.T, ThreadStartStore, func(*testing.T) ThreadStartStore)) {
	t.Helper()
	forSQLStores(t, func(t *testing.T, store sqlStore, reopen func(*testing.T) sqlStore) {
		test(t, store.(ThreadStartStore), func(t *testing.T) ThreadStartStore {
			return reopen(t).(ThreadStartStore)
		})
	})
}

// ThreadStartConversation creates the agent conversation the thread start
// tests post to.
func ThreadStartConversation(t *testing.T, store models.ConversationStore) *models.Conversation {
	t.Helper()
	SeedSpace(t, store.(models.SpaceStore), models.Scope{Kind: "tenant", ID: "thread-start"})
	created, _, err := conversation.NewService(store).CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: "tenant", ID: "thread-start"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
		Title: "Thread start contracts", IdempotencyKey: "thread-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// ThreadStartPost posts req to the conversation at its current revision.
// Unset fields default to a channel question from user "user" whose ID and
// content are its idempotency key.
func ThreadStartPost(t *testing.T, service *conversation.Service, target *models.Conversation, req conversation.PostChannelMessageRequest) *models.ChannelMessage {
	t.Helper()
	current, err := service.GetConversation(t.Context(), target.Scope, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Scope, req.ConversationID, req.ExpectedRevision = target.Scope, target.ID, current.Revision
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

// ThreadStartMessageIDs lists the IDs of messages in order.
func ThreadStartMessageIDs(messages []*models.ChannelMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

// ThreadStartHistoryIDs lists the IDs of the messages of a history page in
// order; a nil page has none.
func ThreadStartHistoryIDs(page *conversation.HistoryReadResult) []string {
	if page == nil {
		return nil
	}
	ids := make([]string, 0, len(page.Messages))
	for _, message := range page.Messages {
		ids = append(ids, message.ID)
	}
	return ids
}

// AgentParticipant is agent id as a conversation participant.
func AgentParticipant(id string) models.ConversationParticipant {
	return models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: id}
}
