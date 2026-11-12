package conversation

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// TestTerminalFailureReplyNamesTheFailedTool keeps the reply case of the old
// fallback test: a failed call that stopped the attempt is named with the
// tool's own answer, wrappers and diagnostics stripped (Run c7a9efec).
func TestTerminalFailureReplyNamesTheFailedTool(t *testing.T) {
	call := &models.ActionCall{ID: "post", SkillID: "skill-slack", Action: "slack-send-message", Status: models.ActionCallStatusFailed,
		SideEffect: skill.SideEffectExternal, Error: "runId=abc Atlas error: execution: slack API error: channel_not_found"}
	want := "I couldn't finish this request because Slack reported: slack API error: channel_not_found. I stopped this attempt. Ask me to try again when you're ready."
	if got := terminalFailureReplyOf(&models.RunFailureExplanation{Code: "action_failed", Message: "An operation failed."}, call, call); got != want {
		t.Fatalf("reply = %q", got)
	}
	// A failure the model already moved past is not the cause of a later stop.
	if got := terminalFailureReplyOf(&models.RunFailureExplanation{Code: "execution_failed"}, nil, call); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("unrelated failure attributed: %q", got)
	}
}
