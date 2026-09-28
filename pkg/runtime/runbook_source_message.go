package runtime

import (
	"context"
	"errors"
)

type runbookMessageReader interface {
	GetConversation(context.Context, Scope, string) (*Conversation, error)
	GetChannelMessage(context.Context, Scope, string, string) (*ChannelMessage, error)
}

func forwardRunbookInvocation(invocation map[string]interface{}, sameAgent bool) map[string]interface{} {
	forwarded := cloneMap(invocation)
	if !sameAgent {
		delete(forwarded, "sourceMessage")
	}
	return forwarded
}

// Preserve only the exact invocation, not the whole conversation or model
// checkpoint. It is task context, never evidence of permission or approval.
func runbookSourceMessage(ctx context.Context, reader runbookMessageReader, run *AgentRun) (map[string]interface{}, error) {
	if run.Kind != RunKindConversation {
		return nil, nil
	}
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	if triggerID == "" {
		return nil, nil
	}
	if reader == nil {
		return nil, errors.New("runbook invocation message is unavailable")
	}
	conversation, err := reader.GetConversation(ctx, run.Scope, run.ConcurrencyKey)
	if err != nil || conversation == nil || conversation.Scope != run.Scope || conversation.Owner != run.Owner {
		return nil, errors.New("runbook invocation conversation is unavailable")
	}
	message, err := reader.GetChannelMessage(ctx, run.Scope, conversation.ID, triggerID)
	if err != nil || message == nil || message.Scope != run.Scope || message.ConversationID != conversation.ID || message.ID != triggerID {
		return nil, errors.New("runbook invocation message is unavailable")
	}
	return map[string]interface{}{"messageId": message.ID, "conversationId": conversation.ID, "content": message.Content,
		"sender": map[string]interface{}{"type": string(message.Sender.Type), "id": message.Sender.ID}}, nil
}
