// Unexported forwarders for declarations moved to github.com/axiom-studio/openseal/pkg/runtime/portfolio,
// kept so that code in github.com/axiom-studio/openseal/pkg/runtime keeps its old names.

package runtime

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

// buildAgentRun forwards to portfolio.BuildAgentRun.
func buildAgentRun(ctx context.Context, store models.PortfolioStore, req models.CreateAgentRunRequest, runID string, now time.Time) (*models.AgentRun, error) {
	return portfolio.BuildAgentRun(ctx, store, req, runID, now)
}

// cloneResourceQuantities forwards to portfolio.CloneResourceQuantities.
func cloneResourceQuantities(values map[string]int) map[string]int {
	return portfolio.CloneResourceQuantities(values)
}

// applyObjectiveUpdate forwards to portfolio.ApplyObjectiveUpdate.
func applyObjectiveUpdate(objective *models.Objective, req UpdateObjectiveRequest) {
	portfolio.ApplyObjectiveUpdate(objective, req)
}
