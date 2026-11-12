package runloop

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Test-only access to run loop internals for the external chat loop tests,
// which use the chat loop fixture shared with tests of other packages and so
// cannot be in-package tests.

type StepContextResult = stepContextResult

func AppendStepContext(ctx context.Context, store models.AgentSessionStore, in agentrun.StepInput, preparers []agentrun.StepPreparer, now time.Time) (*StepContextResult, error) {
	return appendStepContext(ctx, store, in, preparers, now)
}

func GovernedJSONResult(status models.ToolResultStatus, value map[string]interface{}) *models.ToolResult {
	return governedJSONResult(status, value)
}
