package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Resolve the channel through kernel-owned lineage, never model-supplied context
// on a delegated child. Restrict this handoff to the acting Agent's own work in
// that chat.
func conversationWorkOrigin(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	return conversationWorkOriginForState(ctx, runs, conversations, run, false)
}

// workOrigin is conversationWorkOrigin over the service's store.
func (s *ConversationService) workOrigin(ctx context.Context, runs PortfolioStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	return conversationWorkOrigin(ctx, runs, s.store, run)
}

// Accepted-answer receipts may outlive a completed or paused parent. Resolve
// their immutable initiating identity without treating it as a fresh Run grant.
func conversationWorkOriginForState(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun, includeInactiveParents bool) (*AgentRun, *Conversation, *ChannelMessage, error) {
	if run != nil && run.ParentRunID == "" && run.Context[ConversationTaskContextKey] != nil {
		return conversationTaskWorkOrigin(ctx, runs, conversations, run)
	}
	if run != nil && run.Kind == RunKindConversation && run.ParentRunID == "" {
		return foregroundConversationWorkOrigin(ctx, runs, conversations, run)
	}
	if run == nil || run.Kind != RunKindAgentWork || run.ParentRunID == "" || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, nil, nil, nil
	}
	current := run
	seen := map[string]bool{run.ID: true}
	for depth := 0; depth < 64 && current.ParentRunID != ""; depth++ {
		parent, err := runs.GetAgentRun(ctx, run.Scope, current.ParentRunID)
		if err != nil {
			return nil, nil, nil, err
		}
		if parent == nil || seen[parent.ID] || parent.Owner != run.Owner || !includeInactiveParents && (isTerminalAgentRunStatus(parent.Status) || parent.Status == AgentRunStatusPaused) {
			return nil, nil, nil, nil
		}
		seen[parent.ID] = true
		current = parent
		if current.Kind != RunKindConversation {
			continue
		}
		id, _ := current.Context[conversationRunContextConversationID].(string)
		triggerID, _ := current.Context[conversationRunContextTriggerID].(string)
		if id == "" || triggerID == "" {
			return nil, nil, nil, nil
		}
		conversation, err := conversations.GetConversation(ctx, run.Scope, id)
		if errors.Is(err, ErrConversationNotFound) {
			return nil, nil, nil, nil
		}
		if err != nil {
			return nil, nil, nil, err
		}
		agentID := ConversationRunAgentID(conversation, run)
		if conversation == nil || conversation.Status != ConversationStatusActive || conversation.Owner != run.Owner ||
			agentID == "" || ConversationRunAgentID(conversation, current) != agentID {
			return nil, nil, nil, nil
		}
		trigger, err := conversations.GetChannelMessage(ctx, run.Scope, id, triggerID)
		return current, conversation, trigger, err
	}
	return nil, nil, nil, nil
}

// Independent tasks keep their original conversation authority after the source
// chat has ended. Only the canonical task ledger can establish that authority;
// copied context fields on an unrelated root Run never grant conversation access.
func conversationTaskWorkOrigin(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	if run == nil || run.Kind != RunKindAgentWork || (run.Owner.Type != OwnerTypeAgent && run.Owner.Type != OwnerTypeTeam) || run.ParentRunID != "" || conversations == nil {
		return nil, nil, nil, nil
	}
	proof, ok := runs.(ConversationTaskProofStore)
	if !ok {
		return nil, nil, nil, nil
	}
	task, err := proof.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	if errors.Is(err, agentrun.ErrConversationTaskNotFound) {
		return nil, nil, nil, nil
	}
	if err != nil || task == nil {
		return nil, nil, nil, err
	}
	if task.Mode != ConversationTaskModeIndependent || task.TargetAgentID == "" || task.TargetAgentID != strings.TrimSpace(run.AssignedAgentID) {
		return nil, nil, nil, nil
	}
	valid, err := agentrun.VerifyConversationTaskWorkRun(ctx, proof, task, run)
	if err != nil || !valid {
		return nil, nil, nil, err
	}
	source, err := runs.GetAgentRun(ctx, task.Scope, task.SourceRunID)
	if err != nil {
		return nil, nil, nil, err
	}
	conversation, err := conversations.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return nil, nil, nil, err
	}
	if conversation == nil || conversation.Status != ConversationStatusActive {
		return nil, nil, nil, nil
	}
	trigger, err := conversations.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
	if err != nil {
		return nil, nil, nil, err
	}
	actor, valid := conversationMessageInitiatingUser(conversation, trigger)
	if !valid || actor != task.AuthenticatedActor || !validConversationTaskAdmissionSource(source, conversation, trigger, task) {
		return nil, nil, nil, nil
	}
	return source, conversation, trigger, nil
}

// A foreground clarification continues the exact Run scheduled for the original
// human message: an Agent chat's Run, or the Team member Run its routing Run
// started. Reload both its execution identity and original thread; a
// model-authored conversation pointer is insufficient to route a question.
func foregroundConversationWorkOrigin(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	if runs == nil || conversations == nil || run == nil || run.Kind != RunKindConversation || (run.Owner.Type != OwnerTypeAgent && !isTeamMemberRun(run)) ||
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
	if isTeamMemberRun(canonical) {
		if verified, err := verifyTeamMemberRun(ctx, runs, conversation, trigger, canonical); err != nil || !verified {
			return nil, nil, nil, err
		}
		return canonical, conversation, trigger, nil
	}
	expected := conversationAgentRunRequest(conversation, trigger)
	if run.ID != agentrun.RunIDForIdempotencyKey(run.Scope, expected.IdempotencyKey) || run.ConcurrencyKey != expected.ConcurrencyKey ||
		canonical.ConcurrencyKey != expected.ConcurrencyKey || canonical.Context["threadRootMessageId"] != expected.Context["threadRootMessageId"] {
		return nil, nil, nil, nil
	}
	return canonical, conversation, trigger, nil
}
