package agentrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

const SkillRuntimeMaintenanceCheckpointKey = "_kernelSkillRuntimeMaintenance"

type skillRuntimeMaintenanceWaiterStore interface {
	ListCompletedSkillRuntimeMaintenanceWaiters(context.Context, models.Scope, int) ([]*models.AgentRun, error)
}

func (s *WakeService) wakeCompletedSkillRuntimeMaintenances(ctx context.Context, scope models.Scope, at time.Time) (*WakeResult, error) {
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
		delete(checkpoint, SkillRuntimeMaintenanceCheckpointKey)
		id := fmt.Sprintf("skill-runtime-ready:%s:%d", run.ID, run.Revision)
		woken, event, err := s.activity.TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusQueued, Checkpoint: checkpoint,
			Summary: "Skill runtime is ready; continuing with current authorized tools", EventType: "run.skill_runtime_ready",
			Actor: models.ActivityActor{Type: "scheduler", ID: "skill-runtime-maintenance"}, WakeSignalID: id, OccurredAt: &at,
		})
		if errors.Is(err, models.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result.Runs = append(result.Runs, WokenRun{Run: woken, Event: event})
	}
	return result, nil
}
