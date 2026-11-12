package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Test-only access to conversation run internals for the external
// conversation run tests, which park Runs with control's ask_user tool and
// so cannot be in-package tests of the conversation run code.

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
