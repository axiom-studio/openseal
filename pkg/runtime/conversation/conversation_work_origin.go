package conversation

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Resolve the channel through kernel-owned lineage, never model-supplied context
// on a delegated child. Restrict this handoff to the acting Agent's own work in
// that chat.
func WorkOrigin(ctx context.Context, runs models.PortfolioStore, conversations models.ConversationStore, run *models.AgentRun) (*models.AgentRun, *models.Conversation, *models.ChannelMessage, error) {
	return WorkOriginForState(ctx, runs, conversations, run, false)
}

// WorkOrigin is conversationWorkOrigin over the service's store.
func (s *Service) WorkOrigin(ctx context.Context, runs models.PortfolioStore, run *models.AgentRun) (*models.AgentRun, *models.Conversation, *models.ChannelMessage, error) {
	return WorkOrigin(ctx, runs, s.store, run)
}

// Accepted-answer receipts may outlive a completed or paused parent. Resolve
// their immutable initiating identity without treating it as a fresh Run grant.
func WorkOriginForState(ctx context.Context, runs models.PortfolioStore, conversations models.ConversationStore, run *models.AgentRun, includeInactiveParents bool) (*models.AgentRun, *models.Conversation, *models.ChannelMessage, error) {
	if run != nil && run.ParentRunID == "" && run.Context[models.ConversationTaskContextKey] != nil {
		return conversationTaskWorkOrigin(ctx, runs, conversations, run)
	}
	if run != nil && run.Kind == models.RunKindConversation && run.ParentRunID == "" {
		return ForegroundConversationWorkOrigin(ctx, runs, conversations, run)
	}
	if run == nil || run.Kind != models.RunKindAgentWork || run.ParentRunID == "" || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, nil, nil, nil
	}
	current := run
	seen := map[string]bool{run.ID: true}
	for depth := 0; depth < 64 && current.ParentRunID != ""; depth++ {
		parent, err := runs.GetAgentRun(ctx, run.Scope, current.ParentRunID)
		if err != nil {
			return nil, nil, nil, err
		}
		if parent == nil || seen[parent.ID] || parent.Owner != run.Owner || !includeInactiveParents && (models.IsTerminalAgentRunStatus(parent.Status) || parent.Status == models.AgentRunStatusPaused) {
			return nil, nil, nil, nil
		}
		seen[parent.ID] = true
		current = parent
		if current.Kind != models.RunKindConversation {
			continue
		}
		id, _ := current.Context[models.ConversationRunConversationIDContextKey].(string)
		triggerID, _ := current.Context[models.ConversationRunTriggerIDContextKey].(string)
		if id == "" || triggerID == "" {
			return nil, nil, nil, nil
		}
		conversation, err := conversations.GetConversation(ctx, run.Scope, id)
		if errors.Is(err, models.ErrConversationNotFound) {
			return nil, nil, nil, nil
		}
		if err != nil {
			return nil, nil, nil, err
		}
		agentID := RunAgentID(conversation, run)
		if conversation == nil || conversation.Status != models.ConversationStatusActive || conversation.Owner != run.Owner ||
			agentID == "" || RunAgentID(conversation, current) != agentID {
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
func conversationTaskWorkOrigin(ctx context.Context, runs models.PortfolioStore, conversations models.ConversationStore, run *models.AgentRun) (*models.AgentRun, *models.Conversation, *models.ChannelMessage, error) {
	if run == nil || run.Kind != models.RunKindAgentWork || (run.Owner.Type != models.OwnerTypeAgent && run.Owner.Type != models.OwnerTypeTeam) || run.ParentRunID != "" || conversations == nil {
		return nil, nil, nil, nil
	}
	proof, ok := runs.(models.ConversationTaskProofStore)
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
	if task.Mode != models.ConversationTaskModeIndependent || task.TargetAgentID == "" || task.TargetAgentID != strings.TrimSpace(run.AssignedAgentID) {
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
	if conversation == nil || conversation.Status != models.ConversationStatusActive {
		return nil, nil, nil, nil
	}
	trigger, err := conversations.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
	if err != nil {
		return nil, nil, nil, err
	}
	actor, valid := MessageInitiatingUser(conversation, trigger)
	if !valid || actor != task.AuthenticatedActor || !models.IsValidConversationTaskAdmissionSource(source, conversation, trigger, task) {
		return nil, nil, nil, nil
	}
	return source, conversation, trigger, nil
}

// A foreground clarification continues the exact Run scheduled for the original
// human message: an Agent chat's Run, or the Team member Run its routing Run
// started. Reload both its execution identity and original thread; a
// model-authored conversation pointer is insufficient to route a question.
func ForegroundConversationWorkOrigin(ctx context.Context, runs models.PortfolioStore, conversations models.ConversationStore, run *models.AgentRun) (*models.AgentRun, *models.Conversation, *models.ChannelMessage, error) {
	if runs == nil || conversations == nil || run == nil || run.Kind != models.RunKindConversation || (run.Owner.Type != models.OwnerTypeAgent && !models.IsTeamMemberRun(run)) ||
		run.Source != models.RunSourceChat || run.ParentRunID != "" || run.RootRunID != run.ID || run.Context[models.ConversationTaskContextKey] != nil || models.ValidateConversationRun(run) != nil {
		return nil, nil, nil, nil
	}
	canonical, err := runs.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if canonical == nil || canonical.Scope != run.Scope || canonical.Kind != run.Kind || canonical.Owner != run.Owner ||
		canonical.AssignedAgentID != run.AssignedAgentID || canonical.Source != run.Source || canonical.ParentRunID != "" ||
		canonical.RootRunID != run.ID || canonical.Context[models.ConversationTaskContextKey] != nil || models.ValidateConversationRun(canonical) != nil {
		return nil, nil, nil, nil
	}
	conversationID, _ := canonical.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := canonical.Context[models.ConversationRunTriggerIDContextKey].(string)
	if run.Context[models.ConversationRunConversationIDContextKey] != conversationID || run.Context[models.ConversationRunTriggerIDContextKey] != triggerID ||
		run.Context["threadRootMessageId"] != canonical.Context["threadRootMessageId"] {
		return nil, nil, nil, nil
	}
	conversation, err := conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return nil, nil, nil, err
	}
	if conversation == nil || conversation.Scope != run.Scope || conversation.ID != conversationID || conversation.Owner != run.Owner || conversation.Status != models.ConversationStatusActive {
		return nil, nil, nil, nil
	}
	trigger, err := conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, valid := MessageInitiatingUser(conversation, trigger); !valid {
		return nil, nil, nil, nil
	}
	if models.IsTeamMemberRun(canonical) {
		if verified, err := VerifyTeamMemberRun(ctx, runs, conversation, trigger, canonical); err != nil || !verified {
			return nil, nil, nil, err
		}
		return canonical, conversation, trigger, nil
	}
	expected := AgentRunRequest(conversation, trigger)
	if run.ID != agentrun.RunIDForIdempotencyKey(run.Scope, expected.IdempotencyKey) || run.ConcurrencyKey != expected.ConcurrencyKey ||
		canonical.ConcurrencyKey != expected.ConcurrencyKey || canonical.Context["threadRootMessageId"] != expected.Context["threadRootMessageId"] {
		return nil, nil, nil, nil
	}
	return canonical, conversation, trigger, nil
}
