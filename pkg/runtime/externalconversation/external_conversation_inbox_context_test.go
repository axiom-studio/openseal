package externalconversation

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
)

func TestInstallationWideInboxKeepsChannelAndDMContextsSeparate(t *testing.T) {
	ctx := t.Context()
	store, _, endpoint := endpointFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
	endpoint.InstallationWide = true
	worker := &ExternalConversationInboxWorker{store: store, conversations: runtimeconversation.NewConversationService(store), now: time.Now}
	seen := make(map[string]string)
	for _, channel := range []string{"C-public", "C-private", "D-alice", "D-bob"} {
		conversation, _, err := worker.ensureConversation(ctx, endpoint, models.NormalizedExternalConversationEvent{ExternalConversationID: channel})
		if err != nil {
			t.Fatal(err)
		}
		if previous := seen[conversation.ID]; previous != "" {
			t.Fatalf("%s inherited %s context", channel, previous)
		}
		seen[conversation.ID] = channel
		message := conversationruntest.PostMessage(t, worker.conversations, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion, channel+"_SECRET", channel+"-message")
		thread, _, err := worker.ensureConversation(ctx, endpoint, models.NormalizedExternalConversationEvent{ExternalConversationID: channel, ExternalThreadID: "same-thread-id"})
		if err != nil || thread.ID != conversation.ID {
			t.Fatalf("thread escaped its channel context: %#v, %v", thread, err)
		}
		viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: endpoint.Owner.ID}}
		for otherID := range seen {
			if otherID == conversation.ID {
				continue
			}
			if _, err := runtimeconversation.ReadConversationHistory(ctx, worker.conversations, endpoint.Scope, endpoint.Owner, otherID, viewer, runtimeconversation.ConversationHistoryReadRequest{MessageID: message.ID}); err == nil {
				t.Fatal("history reader exposed a message from another channel")
			}
		}
	}
}
