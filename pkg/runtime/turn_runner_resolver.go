package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

type TurnRunnerResolver interface {
	ResolveTurnRunner(ctx context.Context, run *AgentRun) (*agentrun.TurnRunnerBinding, error)
}
