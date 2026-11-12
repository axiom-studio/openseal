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

// conversationOperationSavedOutput is a finished operation's user-facing
// output. A failed or canceled operation has none: the loop writes
// Output.summary only on a final answer, so no partial draft can exist.
func conversationOperationSavedOutput(_ context.Context, _ PortfolioStore, operation *AgentRun) (string, error) {
	if operation == nil || operation.Status != AgentRunStatusCompleted {
		return "", nil
	}
	return conversationOperationSummary(operation.Output), nil
}

// Repair old status-only terminal replies without rerunning the model or
// rewriting history. The same durable recovery key survives restarts.
func (s *ConversationRunScheduler) reconcileConversationOperationOutputs(ctx context.Context, scope Scope, result *ConversationRunReconcileResult) error {
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		runs, err := s.runStore.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindConversation, Statuses: []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusFailed}, Limit: pageSize, Offset: offset})
		if err != nil {
			return err
		}
		for _, run := range runs {
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
			if conversationID == "" || triggerID == "" {
				continue
			}
			key := "conversation-operation-recovered:" + run.ID
			if existing, err := s.conversationStore.FindChannelMessageByIdempotencyKey(ctx, scope, conversationID, key); err != nil {
				return err
			} else if existing != nil {
				continue
			}
			oldKey := conversationRunResponseKey(run, triggerID)
			old, err := s.conversationStore.FindChannelMessageByIdempotencyKey(ctx, scope, conversationID, oldKey)
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
			operation, err := s.runStore.GetAgentRun(ctx, scope, operationID)
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
			content, err := conversationOperationSavedOutput(ctx, s.runStore, operation)
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
			// Only the Agent that posted the reply repairs it, with its own
			// operation's output, while it is still on the chat's roster.
			agentID := ConversationRunAgentID(conversation, run)
			if agentID == "" || old.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: agentID}) || operation.AssignedAgentID != agentID {
				continue
			}
			onRoster, err := AgentOnChatRoster(ctx, teamRosterReader(s.runStore), scope, conversation.Owner, agentID)
			if err != nil {
				return err
			}
			if !onRoster {
				continue
			}
			posted, err := s.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: agentID}, Intent: MessageIntentAnswer,
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
