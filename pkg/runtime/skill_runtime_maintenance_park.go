package runtime

import (
	"context"
	"errors"
	"maps"
)

func (p *AgentRunWorkerPool) parkSkillRuntimeMaintenance(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, cause error) bool {
	var held *SkillRuntimeMaintenanceError
	if !errors.As(cause, &held) {
		return false
	}
	checkpoint := maps.Clone(run.Checkpoint)
	if checkpoint == nil {
		checkpoint = make(map[string]interface{})
	}
	checkpoint[skillRuntimeMaintenanceCheckpointKey] = map[string]interface{}{"skillId": held.Maintenance.SkillID, "operationId": held.Maintenance.OperationID}
	turnID := ""
	if turn != nil {
		turnID = turn.ID
	}
	_, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusWaitingForDependency, LeaseOwner: workerID, Checkpoint: checkpoint,
		WakeCondition: &WakeCondition{Type: skillRuntimeMaintenanceWakeType, Reference: held.Maintenance.SkillID},
		Summary:       "Waiting for the Skill runtime upgrade to finish", EventType: "run.skill_runtime_waiting",
		Actor: ActivityActor{Type: "worker", ID: workerID}, TurnID: turnID, CausationID: turnID,
	})
	if err != nil {
		p.logger.Warnw("failed to park Run for Skill runtime maintenance", "runId", run.ID, "error", err)
	}
	return true
}
