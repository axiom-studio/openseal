package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// TerminalFailureReply renders only kernel-classified facts. Provider messages,
// tool arguments, identifiers and command output are never public reply text.
// A failed attempt does not authorize a correction or another model exchange.
func TerminalFailureReply(code string) string {
	reason := "this attempt encountered an execution failure"
	switch terminalFailureCode(code) {
	case "cap_lookup_not_available":
		reason = "the model requested a capability outside the available set"
	case "cap_lookup_duplicate":
		reason = "the model repeated a capability in the same lookup"
	case "cap_lookup_already_loaded":
		reason = "the model tried to load a capability that was already available"
	case "tool_call_cardinality":
		reason = "the model combined operations that must run separately"
	case "tool_call_identity_missing":
		reason = "the model returned an operation without a valid call identity"
	case "tool_call_not_authorized":
		reason = "the model requested an operation that was not offered for this attempt"
	case "provider_invalid_response":
		reason = "the model returned a response that could not be safely executed"
	case "provider_output_truncated":
		reason = "the model provider stopped before completing its response"
	case "tool_arguments_invalid":
		reason = "the model supplied operation arguments that did not match the required format"
	case "action_review_invalid":
		reason = "the model supplied missing or invalid operation review details"
	case "action_external_identity_invalid":
		reason = "the model supplied missing or invalid identification for an external operation"
	case "final_skill_identity_invalid":
		reason = "the model's final answer cited an unavailable or repeated Skill reference"
	case "conversation_task_review_invalid":
		reason = "the task reviewer returned an invalid review response"
	case "conversation_task_review_failed":
		reason = "the task result did not meet the review requirements"
	case "provider_continuation_failed":
		reason = "the model provider did not supply valid continuation data"
	case "provider_authentication_failed":
		reason = "the model provider rejected the configured credential"
	case "provider_access_denied":
		reason = "the model provider denied access to the configured model"
	case "provider_quota_exhausted":
		reason = "the model provider's quota is exhausted"
	case "workspace_credits_exhausted":
		reason = "this workspace has no model credits available"
	case "workspace_daily_limit_reached":
		reason = "this workspace reached its daily model credit limit"
	case "provider_rate_limited":
		reason = "the model provider temporarily limited requests"
	case "provider_unavailable":
		reason = "the model provider was unavailable"
	case "provider_request_rejected":
		reason = "the model provider rejected the request"
	case "model_configuration_failed":
		reason = "the configured model connection was unavailable or incomplete"
	case "budget_exhausted":
		reason = "this attempt reached its execution budget"
	case "workspace_operation_failed":
		reason = "a Workspace operation failed"
	case "action_admission_failed":
		reason = "the proposed operation could not pass validation or authorization"
	case "action_failed":
		reason = "an operation failed"
	case "dependency_failed":
		reason = "work this request depended on failed"
	}
	return "I couldn't finish this request because " + reason + ". I stopped this attempt. Ask me to try again when you're ready."
}

// Normalize a bounded vocabulary rather than forwarding a host-supplied code.
func terminalFailureCode(code string) string {
	switch strings.TrimSpace(code) {
	case "cap_lookup_not_available", "cap_lookup_duplicate", "cap_lookup_already_loaded", "tool_call_cardinality", "tool_call_identity_missing", "tool_call_not_authorized":
		return strings.TrimSpace(code)
	case "provider_output_truncated", "tool_arguments_invalid", "action_review_invalid", "action_external_identity_invalid", "final_skill_identity_invalid", "conversation_task_review_invalid", "conversation_task_review_failed":
		return strings.TrimSpace(code)
	case "provider_invalid_turn_outcome", "provider_invalid_response":
		return "provider_invalid_response"
	case "provider_tool_continuation_missing", "provider_opaque_context_rejected", "provider_referenced_context_rejected", "provider_continuation_failed":
		return "provider_continuation_failed"
	case "provider_authentication_failed", "provider_access_denied", "provider_quota_exhausted", "provider_rate_limited", "provider_request_rejected":
		return strings.TrimSpace(code)
	case "provider_unavailable", "provider_model_unavailable":
		return "provider_unavailable"
	case "gateway_credits_exhausted", "workspace_credits_exhausted":
		return "workspace_credits_exhausted"
	case "gateway_daily_limit_reached", "workspace_daily_limit_reached":
		return "workspace_daily_limit_reached"
	case "model_credential_unavailable", "requires_model_credential", "model_configuration_failed":
		return "model_configuration_failed"
	case "capability_input_budget_exhausted", "turn_input_budget_exhausted", "turn_output_budget_exhausted", "workspace_turn_budget_exhausted", "workspace_action_budget_exhausted", "workspace_operation_limit", "budget_exhausted":
		return "budget_exhausted"
	case "workspace_operation_failed", "workspace_host_unavailable":
		return "workspace_operation_failed"
	case "action_admission_failed", "action_failed", "dependency_failed":
		return strings.TrimSpace(code)
	default:
		return "execution_failed"
	}
}

func checkpointTerminalFailure(checkpoint map[string]interface{}, code string) map[string]interface{} {
	result := checkpointFinalFailureExplanation(checkpoint, "execution", "The requested work did not complete.")
	failure := result[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	failure["code"] = terminalFailureCode(code)
	return result
}

func terminalFailureCodeFromCheckpoint(checkpoint map[string]interface{}) string {
	if failure, ok := checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{}); ok {
		if code, ok := failure["code"].(string); ok && code != "" {
			return terminalFailureCode(code)
		}
		switch failure["kind"] {
		case "workspace":
			return "workspace_operation_failed"
		case "proposal":
			return "action_admission_failed"
		case "action":
			return "action_failed"
		case "dependency", "delegation":
			return "dependency_failed"
		}
	}
	if _, active := ReadToolFeedbackCorrection(checkpoint); active {
		return "action_failed"
	}
	return "execution_failed"
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
		Content:  TerminalFailureReply(terminalFailureCodeFromCheckpoint(run.Checkpoint)),
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
