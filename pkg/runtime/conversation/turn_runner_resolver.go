package conversation

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

type TurnRunnerResolver interface {
	ResolveTurnRunner(ctx context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error)
}
