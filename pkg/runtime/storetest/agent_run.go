package storetest

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// QueuedAgentRun is the Run that runtime's PortfolioService.CreateAgentRun
// builds for an agent's own work (RunSourceObjective, no objective, no
// parent) at now. The store suites write it with CreateAgentRun, since
// storetest cannot import runtime.
func QueuedAgentRun(scope models.Scope, agentID, goal string, now time.Time) *models.AgentRun {
	id := uuid.NewString()
	return &models.AgentRun{
		ID: id, Kind: models.RunKindAgentWork, Scope: scope, RootRunID: id,
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: agentID}, AssignedAgentID: agentID,
		Goal: goal, Source: models.RunSourceObjective, Status: models.AgentRunStatusQueued,
		AvailableAt: now, QueueEnteredAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Engine: "loop", // runtime.RunEngineLoop: a Run the run loop drives
	}
}
