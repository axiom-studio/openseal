package runtime

import (
	"context"
	"errors"
	"maps"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func (p *AgentRunWorkerPool) parkSkillRuntimeMaintenance(ctx context.Context, workerID string, run *models.AgentRun, turn *models.AgentTurn, cause error) bool {
	var held *models.SkillRuntimeMaintenanceError
	if !errors.As(cause, &held) {
		return false
	}
	checkpoint := maps.Clone(run.Checkpoint)
	if checkpoint == nil {
		checkpoint = make(map[string]interface{})
	}
	checkpoint[agentrun.SkillRuntimeMaintenanceCheckpointKey] = map[string]interface{}{"skillId": held.Maintenance.SkillID, "operationId": held.Maintenance.OperationID}
	turnID := ""
	if turn != nil {
		turnID = turn.ID
	}
	_, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusWaitingForDependency, LeaseOwner: workerID, Checkpoint: checkpoint,
		WakeCondition: &models.WakeCondition{Type: models.SkillRuntimeMaintenanceWakeType, Reference: held.Maintenance.SkillID},
		Summary:       "Waiting for the Skill runtime upgrade to finish", EventType: "run.skill_runtime_waiting",
		Actor: models.ActivityActor{Type: "worker", ID: workerID}, TurnID: turnID, CausationID: turnID,
	})
	if err != nil {
		p.logger.Warnw("failed to park Run for Skill runtime maintenance", "runId", run.ID, "error", err)
	}
	return true
}
