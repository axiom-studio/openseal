package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// ApprovalModeAllowsAction reports whether mode lets an action with this
// classification proceed without an approval checkpoint. An empty mode (a Run
// that acts for no conversation) behaves like manual. An action whose manifest
// declares review: always keeps its checkpoint in every mode, including skip.
func ApprovalModeAllowsAction(mode ConversationApprovalMode, review skill.ActionReview, risk skill.RiskLevel, sideEffect skill.SideEffect) bool {
	if review == skill.ActionReviewAlways {
		return false
	}
	switch mode {
	case ConversationApprovalSkip:
		return true
	case ConversationApprovalAuto:
		return (risk == skill.RiskLevelRead || risk == skill.RiskLevelWrite) &&
			(sideEffect == skill.SideEffectNone || sideEffect == skill.SideEffectRead || sideEffect == skill.SideEffectWrite)
	default:
		return false
	}
}

type conversationReader interface {
	GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error)
}

// RunConversationID returns the canonical conversation a Run acts for: the
// Run's own conversation context, else its root Run's. This is the same
// projection approval listings use to place a checkpoint in a conversation.
func RunConversationID(ctx context.Context, runs agentRunReader, run *AgentRun) (string, error) {
	if run == nil {
		return "", nil
	}
	if id, _ := run.Context[conversationRunContextConversationID].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id), nil
	}
	rootID := strings.TrimSpace(run.RootRunID)
	if rootID == "" || rootID == run.ID || runs == nil {
		return "", nil
	}
	root, err := runs.GetAgentRun(ctx, run.Scope, rootID)
	if errors.Is(err, ErrRunNotFound) || err == nil && root == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id, _ := root.Context[conversationRunContextConversationID].(string)
	return strings.TrimSpace(id), nil
}

// RunConversationApprovalMode resolves the approval mode that governs a Run.
// A Run that acts for no stored conversation has no mode (manual behavior).
// An event routine occurrence, and any Run under one, is always manual
// whatever the conversation's mode: outside events must not widen what runs
// without asking, so only the routine's grants run unasked.
func RunConversationApprovalMode(ctx context.Context, runs agentRunReader, conversations conversationReader, run *AgentRun) (string, ConversationApprovalMode, error) {
	conversationID, err := RunConversationID(ctx, runs, run)
	if err != nil {
		return conversationID, "", err
	}
	if routineEventOccurrenceRun(run) {
		return conversationID, ConversationApprovalManual, nil
	}
	if conversationID == "" || conversations == nil {
		return conversationID, "", nil
	}
	conversation, err := conversations.GetConversation(ctx, run.Scope, conversationID)
	if errors.Is(err, ErrConversationNotFound) || err == nil && conversation == nil {
		return conversationID, "", nil
	}
	if err != nil {
		return conversationID, "", err
	}
	return conversationID, conversationApprovalModeOrDefault(conversation.ApprovalMode), nil
}

// routineEventOccurrenceRun reports whether run is an event routine
// occurrence or runs under one. Child Runs inherit routineEventBatchId from
// their source (inheritRoutineEventBatch), so this reads only run.Context and
// never the store.
func routineEventOccurrenceRun(run *AgentRun) bool {
	return run != nil && routineEventBatchOf(run) != ""
}

func conversationApprovalModeOrDefault(mode ConversationApprovalMode) ConversationApprovalMode {
	if mode == "" {
		return DefaultConversationApprovalMode
	}
	return mode
}
