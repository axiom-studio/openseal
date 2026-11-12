package models

import (
	"context"
	"fmt"
	"strings"
)

// ConversationApprovalMode is the per-conversation review boundary the kernel
// applies to side-effecting actions proposed by Runs acting for that
// conversation. It is enforced by the action policy, never by a client.
type ConversationApprovalMode string

const (
	// ConversationApprovalManual requires approval for every side-effecting action.
	ConversationApprovalManual ConversationApprovalMode = "manual"
	// ConversationApprovalAuto allows read and write risk actions; external,
	// production, and destructive actions still require approval.
	ConversationApprovalAuto ConversationApprovalMode = "auto"
	// ConversationApprovalSkip allows every action without approval. It also
	// means the agent completes browser tasks end to end (signing in, checking
	// out, placing orders, accepting terms, sending) instead of handing off.
	// The action policy remains the authority for each action.
	ConversationApprovalSkip        ConversationApprovalMode = "skip"
	DefaultConversationApprovalMode                          = ConversationApprovalAuto
)

func (m ConversationApprovalMode) Valid() bool {
	switch m {
	case ConversationApprovalManual, ConversationApprovalAuto, ConversationApprovalSkip:
		return true
	}
	return false
}

// Rank orders modes from strictest to loosest.
func (m ConversationApprovalMode) Rank() int {
	switch m {
	case ConversationApprovalAuto:
		return 1
	case ConversationApprovalSkip:
		return 2
	default:
		return 0
	}
}

// ApplyConversationDefaults fills fields introduced after a conversation was
// first persisted, so every stored and returned conversation is explicit.
func ApplyConversationDefaults(conversation *Conversation) {
	if conversation != nil && conversation.ApprovalMode == "" {
		conversation.ApprovalMode = DefaultConversationApprovalMode
	}
}

// ConversationApprovalModeStore atomically commits a conversation revision
// together with the activity event that records it.
type ConversationApprovalModeStore interface {
	UpdateConversationWithEvent(ctx context.Context, conversation *Conversation, expectedRevision int64, event *ActivityEvent) (*Conversation, *ActivityEvent, error)
}

func ValidateConversationEvent(conversation *Conversation, event *ActivityEvent) error {
	if err := conversation.Validate(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if event.Scope != conversation.Scope || event.RunID != "" || event.ObjectiveID != "" ||
		activityConversationSubject(event) != conversation.ID {
		return fmt.Errorf("%w: conversation event must be recorded against the updated conversation", ErrInvalidConversation)
	}
	return nil
}

type ConversationApprovalModeKernelStore interface {
	ConversationStore
	ConversationApprovalModeStore
	PortfolioStore
	ActionStore
}

type AgentRunReader interface {
	GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error)
}

// inheritRoutineEventBatch gives a child Run its source's routineEventBatchId,
// and drops any value the child's caller-supplied context tried to set, so
// only a real occurrence and the Runs under it carry the key.
func InheritRoutineEventBatch(source *AgentRun, context map[string]interface{}) map[string]interface{} {
	batch := ""
	if source != nil {
		batch = RoutineEventBatchID(source)
	}
	if batch == "" {
		delete(context, RoutineEventBatchContextKey)
		return context
	}
	if context == nil {
		context = map[string]interface{}{}
	}
	context[RoutineEventBatchContextKey] = batch
	return context
}

func RoutineEventBatchID(run *AgentRun) string {
	batch, _ := run.Context[RoutineEventBatchContextKey].(string)
	return strings.TrimSpace(batch)
}
