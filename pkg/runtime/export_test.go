package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// Test-only access to conversation run internals for the external
// conversation run tests, which use fixtures of packages above the
// conversation run code (control's ask_user tool, the task source fixture)
// and so cannot be in-package tests of it.

func (s *ConversationRunScheduler) ResumeConversationAnswer(ctx context.Context, conversation *models.Conversation, answer *models.ChannelMessage) (*agentrun.AgentRunCommandResult, bool, error) {
	return s.resumeConversationAnswer(ctx, conversation, answer)
}

func (s *ConversationRunScheduler) ReplaceOtherMemberApprovals(ctx context.Context, conversation *models.Conversation, message *models.ChannelMessage, root string, routed map[string]bool) error {
	return s.replaceOtherMemberApprovals(ctx, conversation, message, root, routed)
}

// Runs is the run command service the scheduler creates Runs with.
func (s *ConversationRunScheduler) Runs() *agentrun.RunCommandService {
	return s.runs
}

// NewPortfolioTurnRunner is a turn runner that only reads Runs from
// portfolio, enough for ActiveConversationRuns.
func NewPortfolioTurnRunner(portfolio models.PortfolioStore) *ConversationRunTurnRunner {
	return &ConversationRunTurnRunner{portfolio: portfolio}
}

func (r *ConversationRunTurnRunner) ActiveConversationRuns(ctx context.Context, conversation *models.Conversation, triggers ...*models.ChannelMessage) ([]runtimeconversation.AgentConversationActiveRun, error) {
	return r.activeConversationRuns(ctx, conversation, triggers...)
}
