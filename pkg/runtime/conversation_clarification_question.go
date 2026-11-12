package runtime

// Use only after conversationWorkOrigin has verified the canonical origin.
func conversationClarificationReferences(run *AgentRun) []ConversationReference {
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
	if run.Kind == RunKindAgentWork && run.ParentRunID == "" {
		if taskID, ok := run.Context[ConversationTaskContextKey].(string); ok && taskID != "" {
			references = append(references, ConversationReference{Kind: ConversationReferenceTask, ID: taskID})
		}
	}
	return references
}

const conversationClarificationKeyPrefix = "conversation-clarification:"

// conversationClarificationKey is the idempotency key of the question message
// an ask_user entry posts.
func conversationClarificationKey(runID, entryID string) string {
	return conversationClarificationKeyPrefix + runID + ":" + entryID
}

// conversationQuestionPostRequest is the one shape of a clarification question
// message; the ask_user settle and the question reconciler post the same
// request, so either replays the other.
func conversationQuestionPostRequest(run *AgentRun, conversation *Conversation, trigger *ChannelMessage, args *askUserArgs, key string) PostChannelMessageRequest {
	return PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: ConversationRunAgentID(conversation, run)},
		Intent: MessageIntentQuestion, Content: args.Question, Question: args.ChannelQuestion(), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		ReplyToMessageID: trigger.ID, RequiresResponse: true, References: conversationClarificationReferences(run),
		IdempotencyKey: key,
	}
}
