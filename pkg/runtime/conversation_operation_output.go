package runtime

import (
	"context"
	"errors"
	"strings"
)

// Only documented user-facing output fields are projected. Never stringify an
// arbitrary runbook result: it may contain internal receipts or configuration.
func conversationOperationSummary(output map[string]interface{}) string {
	if summary, ok := output["summary"].(string); ok && strings.TrimSpace(summary) != "" {
		return strings.TrimSpace(summary)
	}
	if result, ok := output["result"].(map[string]interface{}); ok {
		if summary, ok := result["summary"].(string); ok {
			return strings.TrimSpace(summary)
		}
	}
	return ""
}
func conversationOperationSavedOutput(ctx context.Context, store PortfolioStore, operation *AgentRun) (string, error) {
	if operation.Status == AgentRunStatusCompleted {
		return conversationOperationSummary(operation.Output), nil
	}
	if operation.Status != AgentRunStatusFailed {
		return "", nil
	}
	// A failed runbook may have a valid, persisted writing result from its
	// reasoning child before a later turn failed. It remains partial, not success.
	children, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: operation.Scope, Owner: &operation.Owner, ParentRunID: operation.ID, Limit: 2})
	if err != nil {
		return "", err
	}
	if len(children) != 1 {
		return "", nil
	}
	child := children[0]
	if child.Context[DelegationModeContextKey] != "reason" || child.LastAppliedTurn < 1 {
		return "", nil
	}
	previous, _ := child.Checkpoint[turnContinuityCheckpointKey].(map[string]interface{})
	if previous["nextRunStatus"] == string(AgentRunStatusWaitingForEvent) || previous["nextRunStatus"] == AgentRunStatusWaitingForEvent {
		return "", nil
	}
	if summary := conversationOperationSummary(child.Output); summary != "" {
		return "The task stopped before finishing. This saved draft may be incomplete and needs review:\n\n" + summary, nil
	}
	return "", nil
}

// Repair old status-only terminal replies without rerunning the model or
// rewriting history. The same durable recovery key survives restarts.
func (s *ConversationRunScheduler) reconcileConversationOperationOutputs(ctx context.Context, scope Scope, result *ConversationRunReconcileResult) error {
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		runs, err := s.runs.store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation, Statuses: []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusFailed}, Limit: pageSize, Offset: offset})
		if err != nil {
			return err
		}
		for _, run := range runs {
			if run.Owner.Type != OwnerTypeAgent {
				continue
			}
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
			if conversationID == "" || triggerID == "" {
				continue
			}
			key := "conversation-operation-recovered:" + run.ID
			if existing, err := s.conversations.store.FindChannelMessageByIdempotencyKey(ctx, scope, conversationID, key); err != nil {
				return err
			} else if existing != nil {
				continue
			}
			oldKey := "agent-channel-response:" + hashString(scope.Kind+"\x00"+scope.ID+"\x00"+run.ID+"\x00"+triggerID)
			old, err := s.conversations.store.FindChannelMessageByIdempotencyKey(ctx, scope, conversationID, oldKey)
			if err != nil {
				return err
			}
			if old == nil {
				continue
			}
			operationID := ""
			for _, ref := range old.References {
				if ref.Kind == ConversationReferenceRun {
					operationID = ref.ID
					break
				}
			}
			if operationID == "" {
				continue
			}
			operation, err := s.runs.store.GetAgentRun(ctx, scope, operationID)
			if err != nil {
				return err
			}
			if operation == nil || operation.ParentRunID != run.ID || operation.Owner != run.Owner || operation.Entrypoint == "" {
				continue
			}
			label := "Operation “" + operation.Entrypoint + "”"
			if old.Content != label+" completed successfully." && old.Content != label+" failed. Review the Run for the exact failed step and retry when ready." {
				continue
			}
			content, err := conversationOperationSavedOutput(ctx, s.runs.store, operation)
			if err != nil {
				return err
			}
			if content == "" {
				continue
			}
			conversation, err := s.conversations.GetConversation(ctx, scope, conversationID)
			if errors.Is(err, ErrConversationNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if conversation.Status != ConversationStatusActive || conversation.Owner != run.Owner {
				continue
			}
			posted, err := s.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}, Intent: MessageIntentAnswer,
				Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: triggerID, ResolvesMessageID: triggerID,
				References: []ConversationReference{{Kind: ConversationReferenceRun, ID: operation.ID}}, IdempotencyKey: key,
			})
			if errors.Is(err, ErrRevisionConflict) {
				continue
			}
			if err != nil {
				return err
			}
			if !posted.Replayed {
				result.Results++
			}
		}
		if len(runs) < pageSize {
			return nil
		}
	}
}
