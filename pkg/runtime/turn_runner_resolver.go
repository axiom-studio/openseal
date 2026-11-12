package runtime

import (
	"context"
)

type TurnRunnerResolver interface {
	ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error)
}
