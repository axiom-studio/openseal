package runtime

import (
	"context"
	"maps"
	"strings"
)

// createAgentRunWithAcceptedExecution preserves an already accepted delivery
// before consulting mutable deployment state. New work is prepared through the
// host's authoritative catalog before the canonical creation transaction.
func createAgentRunWithAcceptedExecution(ctx context.Context, commands *RunCommandService, request CreateAgentRunRequest, preparer AcceptedRunExecutionPreparer) (*AgentRunCommandResult, error) {
	if preparer != nil {
		request.Context = maps.Clone(request.Context)
		delete(request.Context, AcceptedRunExecutionContextKey)
	}
	if strings.TrimSpace(request.IdempotencyKey) != "" {
		existing, err := commands.FindCreatedAgentRun(ctx, request)
		if err != nil || existing != nil {
			return existing, err
		}
	}
	if preparer != nil {
		if err := preparer.PrepareAcceptedRunExecution(ctx, &request); err != nil {
			return nil, err
		}
	}
	return commands.CreateAgentRun(ctx, request)
}
