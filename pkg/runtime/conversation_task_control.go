package runtime

import "context"

// A task control uses the normal governed Run action, with the task's exact
// conversation/thread and initiating human verified again at execution time.
func validateConversationTaskControl(ctx context.Context, store RunCommandStore, source, target *AgentRun) error {
	if target == nil || target.Context[ConversationTaskContextKey] == nil {
		return nil
	}
	tasks, ok := store.(ConversationTaskStore)
	if !ok {
		return ErrInvalidConversationTask
	}
	task, err := PersistedTaskOrigin(ctx, tasks, target.Scope, target.ID)
	if err != nil {
		return err
	}
	conversations, ok := store.(ConversationStore)
	if !ok || source == nil || source.Scope != task.Scope || source.Owner != task.Owner || source.Kind != RunKindConversation || source.Source != RunSourceChat ||
		source.Context[conversationRunContextConversationID] != task.ConversationID {
		return ErrInvalidConversationTask
	}
	conversation, err := conversations.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return err
	}
	messageID, _ := source.Context[conversationRunContextTriggerID].(string)
	message, err := conversations.GetChannelMessage(ctx, task.Scope, task.ConversationID, messageID)
	if err != nil {
		return err
	}
	if conversation == nil || message == nil {
		return ErrInvalidConversationTask
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor != task.AuthenticatedActor {
		return ErrInvalidConversationTask
	}
	if thread := externalConversationThreadRoot(conversation, message); thread != "" && thread != task.ThreadRootID {
		return ErrInvalidConversationTask
	}
	return nil
}
