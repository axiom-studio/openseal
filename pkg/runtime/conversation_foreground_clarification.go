package runtime

import "context"

// A foreground clarification continues the exact Run scheduled for the original
// human message. Reload both its execution identity and original thread; a
// model-authored conversation pointer is insufficient to route a question.
func foregroundConversationWorkOrigin(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	if runs == nil || conversations == nil || run == nil || run.Kind != RunKindConversation || run.Owner.Type != OwnerTypeAgent ||
		run.Source != RunSourceChat || run.ParentRunID != "" || run.RootRunID != run.ID || run.Context[ConversationTaskContextKey] != nil || validateConversationRun(run) != nil {
		return nil, nil, nil, nil
	}
	canonical, err := runs.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if canonical == nil || canonical.Scope != run.Scope || canonical.Kind != run.Kind || canonical.Owner != run.Owner ||
		canonical.AssignedAgentID != run.AssignedAgentID || canonical.Source != run.Source || canonical.ParentRunID != "" ||
		canonical.RootRunID != run.ID || canonical.Context[ConversationTaskContextKey] != nil || validateConversationRun(canonical) != nil {
		return nil, nil, nil, nil
	}
	conversationID, _ := canonical.Context[conversationRunContextConversationID].(string)
	triggerID, _ := canonical.Context[conversationRunContextTriggerID].(string)
	if run.Context[conversationRunContextConversationID] != conversationID || run.Context[conversationRunContextTriggerID] != triggerID ||
		run.Context["threadRootMessageId"] != canonical.Context["threadRootMessageId"] {
		return nil, nil, nil, nil
	}
	conversation, err := conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return nil, nil, nil, err
	}
	if conversation == nil || conversation.Scope != run.Scope || conversation.ID != conversationID || conversation.Owner != run.Owner || conversation.Status != ConversationStatusActive {
		return nil, nil, nil, nil
	}
	trigger, err := conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, valid := conversationMessageInitiatingUser(conversation, trigger); !valid {
		return nil, nil, nil, nil
	}
	expected := conversationAgentRunRequest(conversation, trigger)
	if run.ID != runIDForIdempotencyKey(run.Scope, expected.IdempotencyKey) || run.ConcurrencyKey != expected.ConcurrencyKey ||
		canonical.ConcurrencyKey != expected.ConcurrencyKey || canonical.Context["threadRootMessageId"] != expected.Context["threadRootMessageId"] {
		return nil, nil, nil, nil
	}
	return canonical, conversation, trigger, nil
}
