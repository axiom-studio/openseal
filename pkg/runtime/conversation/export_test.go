package conversation

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
)

// Test-only access to conversation internals for the external conversation
// tests, which use fixtures shared with tests of other packages
// (conversationtest) and so cannot be in-package tests.

const (
	MaximumCatchUpMessages       = maximumCatchUpMessages
	CatchUpReservedAppendEntries = catchUpReservedAppendEntries
)

func LoadConversationThreadBackdrop(ctx context.Context, service *ConversationService, conversation *models.Conversation, trigger *models.ChannelMessage, viewer models.ConversationViewer) ([]*models.ChannelMessage, error) {
	return loadConversationThreadBackdrop(ctx, service, conversation, trigger, viewer)
}

func (s *ConversationChangeService) FilterVisibleRuns(ctx context.Context, scope models.Scope, conversationID string, runs []*models.AgentRun, viewer models.ConversationViewer) ([]*models.AgentRun, error) {
	return s.filterVisibleRuns(ctx, scope, conversationID, runs, viewer)
}

// Approvals is the approval coordinator the service resolves approvals
// through.
func (s *ConversationApprovalModeService) Approvals() *action.ApprovalCoordinator {
	return s.approvals
}
