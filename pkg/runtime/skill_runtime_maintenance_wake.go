package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
		checkpoint := maps.Clone(run.Checkpoint)
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
