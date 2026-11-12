package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Test-only access to control internals for the external ask_user tests,
// which use fixtures shared with tests of other packages and so cannot be
// in-package tests.

const AskUserWaitReason = askUserWaitReason

func AskUserAnswer(run *models.AgentRun, entry *models.SessionEntry, questionMessageID string) *models.ConversationAnswerReceipt {
	return askUserAnswer(run, entry, questionMessageID)
}

func (x *ControlExecutor) TransitionControlEntry(ctx context.Context, cc agentrun.ControlContext, to models.ToolEntryState, wait *models.ToolEntryWait, result *models.ToolResult, meta *ControlEntryMeta) (*models.SessionEntry, error) {
	return x.transitionControlEntry(ctx, cc, to, wait, result, meta)
}
