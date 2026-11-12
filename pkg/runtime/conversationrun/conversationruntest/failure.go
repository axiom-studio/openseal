package conversationruntest

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/google/uuid"
)

// RunConversationStore holds the runs, their activity and the conversations
// the failed conversation run fixture writes.
type RunConversationStore interface {
	models.RunCommandStore
	models.ConversationStore
}

// TerminalFailureConversation creates a conversation whose person asks a
// question that needs a spoken answer, and the queued conversation Run of
// the agent "finn" that answers it, created at at. With private the question
// is addressed to the agent only.
func TerminalFailureConversation(t *testing.T, store RunConversationStore, at time.Time, private bool) (*models.AgentRun, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "failure-" + uuid.NewString()}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"}
	service := runtimeconversation.NewService(store)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Finn", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	audience := models.ConversationAudience{Kind: models.ConversationAudienceChannel}
	if private {
		audience = models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantAgent, ID: owner.ID}}}
	}
	posted, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "kev"}, Intent: models.MessageIntentQuestion,
		Content: "Check my recent emails", Audience: audience, StartThread: true, ResponseMode: "spoken",
		RequiresResponse: true, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	commands := agentrun.NewRunCommandService(store, models.WithClock(func() time.Time { return at }))
	created, err := commands.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Kind: models.RunKindConversation, Source: models.RunSourceChat,
		Goal: "Check the recent email request", ConcurrencyKey: conversation.ID + ":thread:" + posted.Message.ID,
		Context:        map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: posted.Message.ID, "threadRootMessageId": posted.Message.ID},
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.Run, posted.Conversation, posted.Message
}

// FailTerminalConversation fails run with code, which must already be a
// classified reply code (TerminalFailureReply's vocabulary), as the loop
// records it.
func FailTerminalConversation(t *testing.T, store RunConversationStore, run *models.AgentRun, at time.Time, code string) *models.AgentRun {
	t.Helper()
	activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return at }))
	running, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusFailed,
		Error:              "private-provider-diagnostic token=NEVER-PUBLISH",
		FailureExplanation: &models.RunFailureExplanation{Kind: "execution", Code: code, Message: "The requested work did not complete."},
	})
	if err != nil {
		t.Fatal(err)
	}
	return failed
}
