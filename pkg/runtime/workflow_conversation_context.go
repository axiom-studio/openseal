package runtime

import (
	"context"
	"fmt"
)

// Deferred workflows outlive the chat Run that registered them. Their origin
// is a pinned human request, not a live parent dependency. Reload that exact
// request through canonical scoped records before exposing its wording to a
// resumed Turn; model-authored checkpoint/context cannot select another chat.
func deferredWorkflowConversationOrigin(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	invalid := func() (*AgentRun, *Conversation, *ChannelMessage, error) {
		return nil, nil, nil, fmt.Errorf("%w: deferred workflow origin does not match its pinned human request", ErrInvalidAgentRun)
	}
	if runs == nil || conversations == nil || run == nil || run.Kind != RunKindAgentWork || run.ParentRunID != "" || run.Context[deferredWorkflowContextKey] != true || run.Context[scheduledTaskContextKey] != true || run.AssignedAgentID == "" || run.Owner.Validate() != nil || (run.Owner.Type == OwnerTypeAgent && run.Owner.ID != run.AssignedAgentID) {
		return invalid()
	}
	sourceID, _ := run.Context["workflowSourceRunId"].(string)
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	if !validOpaqueIdentifier(sourceID, 128) || sourceID == run.ID || !validOpaqueIdentifier(conversationID, 256) || !validOpaqueIdentifier(triggerID, 256) {
		return invalid()
	}
	source, err := runs.GetAgentRun(ctx, run.Scope, sourceID)
	if err != nil {
		return nil, nil, nil, err
	}
	if source == nil || source.Scope != run.Scope || source.ID != sourceID || source.Kind != RunKindConversation || source.Owner != run.Owner || source.AssignedAgentID != run.AssignedAgentID || source.Context[conversationRunContextConversationID] != conversationID || source.Context[conversationRunContextTriggerID] != triggerID {
		return invalid()
	}
	conversation, err := conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return nil, nil, nil, err
	}
	if conversation == nil || conversation.Scope != run.Scope || conversation.ID != conversationID || conversation.Owner != run.Owner || conversation.Status != ConversationStatusActive {
		return invalid()
	}
	trigger, err := conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return nil, nil, nil, err
	}
	if trigger == nil || trigger.Scope != run.Scope || trigger.ID != triggerID || trigger.ConversationID != conversationID || trigger.Sender.Type != ConversationParticipantUser || trigger.Audience.Kind != ConversationAudienceChannel {
		return invalid()
	}
	return source, conversation, trigger, nil
}
