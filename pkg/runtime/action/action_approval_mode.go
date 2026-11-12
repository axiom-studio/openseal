package action

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// ApprovalModeAllowsAction reports whether mode lets an action with this
// classification proceed without an approval checkpoint. An empty mode (a Run
// that acts for no conversation) behaves like manual. An action whose manifest
// declares review: always keeps its checkpoint in every mode, including skip.
func ApprovalModeAllowsAction(mode models.ConversationApprovalMode, review skill.ActionReview, risk skill.RiskLevel, sideEffect skill.SideEffect) bool {
	if review == skill.ActionReviewAlways {
		return false
	}
	switch mode {
	case models.ConversationApprovalSkip:
		return true
	case models.ConversationApprovalAuto:
		return (risk == skill.RiskLevelRead || risk == skill.RiskLevelWrite) &&
			(sideEffect == skill.SideEffectNone || sideEffect == skill.SideEffectRead || sideEffect == skill.SideEffectWrite)
	default:
		return false
	}
}

// ConversationReader reads the conversation a Run acts for.
type ConversationReader interface {
	GetConversation(ctx context.Context, scope models.Scope, conversationID string) (*models.Conversation, error)
}

// RunConversationID returns the canonical conversation a Run acts for: the
// Run's own conversation context, else its root Run's. This is the same
// projection approval listings use to place a checkpoint in a conversation.
func RunConversationID(ctx context.Context, runs models.AgentRunReader, run *models.AgentRun) (string, error) {
	if run == nil {
		return "", nil
	}
	if id, _ := run.Context[models.ConversationRunConversationIDContextKey].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id), nil
	}
	rootID := strings.TrimSpace(run.RootRunID)
	if rootID == "" || rootID == run.ID || runs == nil {
		return "", nil
	}
	root, err := runs.GetAgentRun(ctx, run.Scope, rootID)
	if errors.Is(err, models.ErrRunNotFound) || err == nil && root == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id, _ := root.Context[models.ConversationRunConversationIDContextKey].(string)
	return strings.TrimSpace(id), nil
}

// RunConversationApprovalMode resolves the approval mode that governs a Run.
// A Run that acts for no stored conversation has no mode (manual behavior).
// An event routine occurrence, and any Run under one, is always manual
// whatever the conversation's mode: outside events must not widen what runs
// without asking, so only the routine's grants run unasked.
func RunConversationApprovalMode(ctx context.Context, runs models.AgentRunReader, conversations ConversationReader, run *models.AgentRun) (string, models.ConversationApprovalMode, error) {
	conversationID, err := RunConversationID(ctx, runs, run)
	if err != nil {
		return conversationID, "", err
	}
	if IsRoutineEventOccurrenceRun(run) {
		return conversationID, models.ConversationApprovalManual, nil
	}
	if conversationID == "" || conversations == nil {
		return conversationID, "", nil
	}
	conversation, err := conversations.GetConversation(ctx, run.Scope, conversationID)
	if errors.Is(err, models.ErrConversationNotFound) || err == nil && conversation == nil {
		return conversationID, "", nil
	}
	if err != nil {
		return conversationID, "", err
	}
	return conversationID, ConversationApprovalModeOrDefault(conversation.ApprovalMode), nil
}

// IsRoutineEventOccurrenceRun reports whether run is an event routine
// occurrence or runs under one. Child Runs inherit routineEventBatchId from
// their source (inheritRoutineEventBatch), so this reads only run.Context and
// never the store.
func IsRoutineEventOccurrenceRun(run *models.AgentRun) bool {
	return run != nil && models.RoutineEventBatchID(run) != ""
}

// ConversationApprovalModeOrDefault is mode, or the default approval mode when
// a conversation has none.
func ConversationApprovalModeOrDefault(mode models.ConversationApprovalMode) models.ConversationApprovalMode {
	if mode == "" {
		return models.DefaultConversationApprovalMode
	}
	return mode
}
