package conversation

import "github.com/axiom-studio/openseal/pkg/models"

// Use only after conversationWorkOrigin has verified the canonical origin.
func ConversationClarificationReferences(run *models.AgentRun) []models.ConversationReference {
	references := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}
	if run.Kind == models.RunKindAgentWork && run.ParentRunID == "" {
		if taskID, ok := run.Context[models.ConversationTaskContextKey].(string); ok && taskID != "" {
			references = append(references, models.ConversationReference{Kind: models.ConversationReferenceTask, ID: taskID})
		}
	}
	return references
}

const conversationClarificationKeyPrefix = "conversation-clarification:"

// ConversationClarificationKey is the idempotency key of the question message
// an ask_user entry posts.
func ConversationClarificationKey(runID, entryID string) string {
	return conversationClarificationKeyPrefix + runID + ":" + entryID
}

// ConversationQuestionPostRequest is the one shape of a clarification question
// message; the ask_user settle and the question reconciler post the same
// request, so either replays the other.
func ConversationQuestionPostRequest(run *models.AgentRun, conversation *models.Conversation, trigger *models.ChannelMessage, args *models.AskUserArgs, key string) PostChannelMessageRequest {
	return PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: ConversationRunAgentID(conversation, run)},
		Intent: models.MessageIntentQuestion, Content: args.Question, Question: args.ChannelQuestion(), Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		ReplyToMessageID: trigger.ID, RequiresResponse: true, References: ConversationClarificationReferences(run),
		IdempotencyKey: key,
	}
}
