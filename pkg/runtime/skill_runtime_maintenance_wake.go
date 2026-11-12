package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const skillRuntimeMaintenanceCheckpointKey = "_kernelSkillRuntimeMaintenance"

type skillRuntimeMaintenanceWaiterStore interface {
	ListCompletedSkillRuntimeMaintenanceWaiters(context.Context, Scope, int) ([]*AgentRun, error)
}

func (s *AgentRunWakeService) wakeCompletedSkillRuntimeMaintenances(ctx context.Context, scope Scope, at time.Time) (*WakeResult, error) {
	store, ok := s.portfolio.(skillRuntimeMaintenanceWaiterStore)
	if !ok {
		return &WakeResult{}, nil
	}
	runs, err := store.ListCompletedSkillRuntimeMaintenanceWaiters(ctx, scope, 200)
	if err != nil {
		return nil, err
	}
	result := &WakeResult{Runs: make([]WokenRun, 0, len(runs))}
	for _, run := range runs {
		checkpoint := cloneMap(run.Checkpoint)
		delete(checkpoint, skillRuntimeMaintenanceCheckpointKey)
		id := fmt.Sprintf("skill-runtime-ready:%s:%d", run.ID, run.Revision)
		woken, event, err := s.activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusQueued, Checkpoint: checkpoint,
			Summary: "Skill runtime is ready; continuing with current authorized tools", EventType: "run.skill_runtime_ready",
			Actor: ActivityActor{Type: "scheduler", ID: "skill-runtime-maintenance"}, WakeSignalID: id, OccurredAt: &at,
		})
		if errors.Is(err, ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result.Runs = append(result.Runs, WokenRun{Run: woken, Event: event})
	}
	return result, nil
}

func (p *AgentRunWorkerPool) parkSkillRuntimeMaintenance(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, cause error) bool {
	var held *SkillRuntimeMaintenanceError
	if !errors.As(cause, &held) {
		return false
	}
	checkpoint := cloneMap(run.Checkpoint)
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
