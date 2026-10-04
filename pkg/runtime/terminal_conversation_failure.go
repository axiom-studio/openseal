package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

func checkpointTerminalFailure(checkpoint map[string]interface{}, code string) map[string]interface{} {
	result := checkpointFinalFailureExplanation(checkpoint, "execution", "The requested work did not complete.")
	failure := result[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	failure["code"] = terminalFailureCode(code)
	return result
}

func terminalFailureCheckpointActive(checkpoint map[string]interface{}) bool {
	if requiresFinalFailureExplanation(checkpoint) {
		return true
	}
	_, active := ReadToolFeedbackCorrection(checkpoint)
	return active
}

func terminalFailureOutcome(run *AgentRun) (*TurnOutcome, bool) {
	if run == nil || !requiresFinalFailureExplanation(run.Checkpoint) {
		if run == nil {
			return nil, false
		}
		if _, active := ReadToolFeedbackCorrection(run.Checkpoint); !active {
			return nil, false
		}
	}
	code := terminalFailureCodeFromCheckpoint(run.Checkpoint)
	checkpoint := checkpointTerminalFailure(run.Checkpoint, code)
	return &TurnOutcome{
		NextRunStatus: AgentRunStatusCompleted, OutputSummary: TerminalFailureReply(code),
		RunOutput:              map[string]interface{}{"summary": TerminalFailureReply(code)},
		ContinuationCheckpoint: checkpoint, ModelProvider: "host", Model: "terminal-failure-report",
	}, true
}

func failedConversationReportCandidate(run *AgentRun) bool {
	if run == nil || run.Status != AgentRunStatusFailed || run.Kind != RunKindConversation || run.ParentRunID != "" {
		return false
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	return validOpaqueIdentifier(conversationID, 128) && validOpaqueIdentifier(triggerID, 128)
}

func terminalConversationFailureReplyKey(run *AgentRun) string {
	return "conversation-failure:" + run.ID + ":" + strconv.FormatInt(run.Revision, 10)
}

// projectFailedConversationReply first checks the normal Agent response key.
// Otherwise outbox recovery and external delivery share one immutable failure
// key per terminal attempt rather than creating additional status messages.
func projectFailedConversationReply(ctx context.Context, store ConversationStore, run *AgentRun) error {
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := store.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	if conversation == nil || conversation.Scope != run.Scope || conversation.Owner != run.Owner || conversation.Status != ConversationStatusActive {
		return ErrInvalidAgentRun
	}
	trigger, err := store.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return err
	}
	if trigger == nil || trigger.Scope != run.Scope || trigger.ConversationID != conversationID || trigger.ID != triggerID || !conversationMessageStartsRun(conversation, trigger) {
		return ErrInvalidAgentRun
	}
	initiator := trigger.Sender
	if voiceCallStartedMessage(trigger) && trigger.Initiator != nil {
		initiator = *trigger.Initiator
	}
	if initiator.Type != ConversationParticipantUser || initiator.Validate() != nil {
		return ErrInvalidAgentRun
	}
	expectedRoot := externalConversationThreadRoot(conversation, trigger)
	if root, _ := run.Context["threadRootMessageId"].(string); root != expectedRoot {
		return ErrInvalidAgentRun
	}
	agentID := strings.TrimSpace(run.AssignedAgentID)
	if run.Owner.Type == OwnerTypeAgent {
		if agentID == "" {
			agentID = run.Owner.ID
		} else if agentID != run.Owner.ID {
			return ErrInvalidAgentRun
		}
	}
	if !validOpaqueIdentifier(agentID, 256) {
		return ErrInvalidAgentRun
	}
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
	if run.Context[ConversationTaskContextKey] != nil {
		task, source, taskErr := conversationTaskForReport(ctx, store, run)
		if taskErr != nil {
			return taskErr
		}
		if task == nil || source || task.Mode != ConversationTaskModeContinuation {
			return ErrInvalidConversationTask
		}
		if saved, findErr := FindConversationTaskResultMessage(ctx, store, task, run); findErr != nil || saved != nil {
			return findErr
		}
		references = append(references, ConversationReference{Kind: ConversationReferenceTask, ID: task.ID})
	}
	// A model answer committed before terminalization already closes this
	// attempt. A failure answer from an older manual attempt does not.
	if saved, findErr := store.FindChannelMessageByIdempotencyKey(ctx, run.Scope, conversationID, conversationTaskFinalResponseKey(run)); findErr != nil && !errors.Is(findErr, ErrChannelMessageNotFound) {
		return findErr
	} else if saved != nil {
		if !terminalConversationReplyMatches(saved, run, triggerID, agentID) {
			return ErrInvalidAgentRun
		}
		return nil
	}
	key := terminalConversationFailureReplyKey(run)
	if saved, findErr := store.FindChannelMessageByIdempotencyKey(ctx, run.Scope, conversationID, key); findErr != nil && !errors.Is(findErr, ErrChannelMessageNotFound) {
		return findErr
	} else if saved != nil {
		if !terminalConversationReplyMatches(saved, run, triggerID, agentID) {
			return ErrInvalidAgentRun
		}
		return nil
	}
	request := PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: agentID}, Intent: MessageIntentAnswer,
		Content:  terminalRunFailureReply(run),
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: triggerID, ResolvesMessageID: triggerID,
		BroadcastToChannel: trigger.ThreadRootID == "" || trigger.BroadcastToChannel, ResponseMode: trigger.ResponseMode,
		References: references, IdempotencyKey: key,
	}
	service := NewConversationService(store)
	_, err = service.PostChannelMessage(ctx, request)
	if errors.Is(err, ErrRevisionConflict) {
		current, getErr := store.GetConversation(ctx, run.Scope, conversationID)
		if getErr != nil {
			return getErr
		}
		request.ExpectedRevision = current.Revision
		_, err = service.PostChannelMessage(ctx, request)
	}
	return err
}

func terminalConversationReplyMatches(message *ChannelMessage, run *AgentRun, triggerID, agentID string) bool {
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	if message == nil || message.Scope != run.Scope || message.ConversationID != conversationID || message.ReplyToMessageID != triggerID || message.Sender.Type != ConversationParticipantAgent || message.Sender.ID != agentID || message.Intent != MessageIntentAnswer || message.Historical || strings.TrimSpace(message.Content) == "" {
		return false
	}
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceRun && reference.ID == run.ID {
			return true
		}
	}
	return false
}
