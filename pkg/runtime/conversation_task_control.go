package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

// A task control uses the normal governed Run action, with the task's exact
// conversation/thread and initiating human verified again at execution time.
func validateConversationTaskControl(ctx context.Context, store models.RunCommandStore, source, target *models.AgentRun) error {
	if target == nil || target.Context[models.ConversationTaskContextKey] == nil {
		return nil
	}
	tasks, ok := store.(models.ConversationTaskStore)
	if !ok {
		return models.ErrInvalidConversationTask
	}
	task, err := PersistedTaskOrigin(ctx, tasks, target.Scope, target.ID)
	if err != nil {
		return err
	}
	conversations, ok := store.(models.ConversationStore)
	if !ok || source == nil || source.Scope != task.Scope || source.Owner != task.Owner || source.Kind != models.RunKindConversation || source.Source != models.RunSourceChat ||
		source.Context[models.ConversationRunConversationIDContextKey] != task.ConversationID {
		return models.ErrInvalidConversationTask
	}
	conversation, err := conversations.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return err
	}
	messageID, _ := source.Context[models.ConversationRunTriggerIDContextKey].(string)
	message, err := conversations.GetChannelMessage(ctx, task.Scope, task.ConversationID, messageID)
	if err != nil {
		return err
	}
	if conversation == nil || message == nil {
		return models.ErrInvalidConversationTask
	}
	actor := message.Sender
	if actor.Type == models.ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor != task.AuthenticatedActor {
		return models.ErrInvalidConversationTask
	}
	if thread := models.ExternalConversationThreadRoot(conversation, message); thread != "" && thread != task.ThreadRootID {
		return models.ErrInvalidConversationTask
	}
	return nil
}
